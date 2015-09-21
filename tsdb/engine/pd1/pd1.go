package pd1

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"io/ioutil"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/golang/snappy"
	"github.com/influxdb/influxdb/models"
	"github.com/influxdb/influxdb/tsdb"
)

const (
	// Format is the file format name of this engine.
	Format = "pd1"

	// FieldsFileExtension is the extension for the file that stores compressed field
	// encoding data for this db
	FieldsFileExtension = "fields"

	// SeriesFileExtension is the extension for the file that stores the compressed
	// series metadata for series in this db
	SeriesFileExtension = "series"

	CollisionsFileExtension = "collisions"
)

type TimePrecision uint8

const (
	Seconds TimePrecision = iota
	Milliseconds
	Microseconds
	Nanoseconds
)

func init() {
	tsdb.RegisterEngine(Format, NewEngine)
}

const (
	MaxDataFileSize = 1024 * 1024 * 1024 // 1GB

	// DefaultRotateBlockSize is the default size to rotate to a new compressed block
	DefaultRotateBlockSize = 512 * 1024 // 512KB

	DefaultRotateFileSize = 5 * 1024 * 1024 // 5MB

	DefaultMaxPointsPerBlock = 1000

	// MAP_POPULATE is for the mmap syscall. For some reason this isn't defined in golang's syscall
	MAP_POPULATE = 0x8000

	magicNumber uint32 = 0x16D116D1
)

// Ensure Engine implements the interface.
var _ tsdb.Engine = &Engine{}

// Engine represents a storage engine with compressed blocks.
type Engine struct {
	writeLock *writeLock
	metaLock  sync.Mutex
	path      string

	// deletesPending mark how many old data files are waiting to be deleted. This will
	// keep a close from returning until all deletes finish
	deletesPending sync.WaitGroup

	// HashSeriesField is a function that takes a series key and a field name
	// and returns a hash identifier. It's not guaranteed to be unique.
	HashSeriesField func(key string) uint64

	WAL *Log

	RotateFileSize                 uint32
	SkipCompaction                 bool
	CompactionAge                  time.Duration
	CompactionFileCount            int
	IndexCompactionFullAge         time.Duration
	IndexMinimumCompactionInterval time.Duration

	// filesLock is only for modifying and accessing the files slice
	filesLock          sync.RWMutex
	files              dataFiles
	currentFileID      int
	compactionRunning  bool
	lastCompactionTime time.Time

	collisionsLock sync.RWMutex
	collisions     map[string]uint64

	// queryLock keeps data files from being deleted or the store from
	// being closed while queries are running
	queryLock sync.RWMutex
}

// NewEngine returns a new instance of Engine.
func NewEngine(path string, walPath string, opt tsdb.EngineOptions) tsdb.Engine {
	w := NewLog(path)
	w.FlushColdInterval = time.Duration(opt.Config.WALFlushColdInterval)
	w.FlushMemorySizeThreshold = opt.Config.WALFlushMemorySizeThreshold
	w.MaxMemorySizeThreshold = opt.Config.WALMaxMemorySizeThreshold
	w.LoggingEnabled = opt.Config.WALLoggingEnabled

	e := &Engine{
		path:      path,
		writeLock: &writeLock{},

		// TODO: this is the function where we can inject a check against the in memory collisions
		HashSeriesField:                hashSeriesField,
		WAL:                            w,
		RotateFileSize:                 DefaultRotateFileSize,
		CompactionAge:                  opt.Config.IndexCompactionAge,
		CompactionFileCount:            opt.Config.IndexCompactionFileCount,
		IndexCompactionFullAge:         opt.Config.IndexCompactionFullAge,
		IndexMinimumCompactionInterval: opt.Config.IndexMinimumCompactionInterval,
	}
	e.WAL.Index = e

	return e
}

// Path returns the path the engine was opened with.
func (e *Engine) Path() string { return e.path }

// PerformMaintenance is for periodic maintenance of the store. A no-op for b1
func (e *Engine) PerformMaintenance() {
	if f := e.WAL.shouldFlush(); f != noFlush {
		go func() {
			e.WAL.flush(f)
		}()
	} else if e.shouldCompact() {
		go e.Compact(true)
	}
}

// Format returns the format type of this engine
func (e *Engine) Format() tsdb.EngineFormat {
	return tsdb.PD1Format
}

// Open opens and initializes the engine.
func (e *Engine) Open() error {
	if err := os.MkdirAll(e.path, 0777); err != nil {
		return err
	}

	// TODO: clean up previous series write
	// TODO: clean up previous fields write
	// TODO: clean up previous names write
	// TODO: clean up any data files that didn't get cleaned up
	// TODO: clean up previous collisions write

	files, err := filepath.Glob(filepath.Join(e.path, fmt.Sprintf("*.%s", Format)))
	if err != nil {
		return err
	}
	for _, fn := range files {
		id, err := idFromFileName(fn)
		if err != nil {
			return err
		}
		if id >= e.currentFileID {
			e.currentFileID = id + 1
		}
		f, err := os.OpenFile(fn, os.O_RDONLY, 0666)
		if err != nil {
			return fmt.Errorf("error opening file %s: %s", fn, err.Error())
		}
		df, err := NewDataFile(f)
		if err != nil {
			return fmt.Errorf("error opening memory map for file %s: %s", fn, err.Error())
		}
		e.files = append(e.files, df)
	}
	sort.Sort(e.files)

	if err := e.WAL.Open(); err != nil {
		return err
	}

	if err := e.readCollisions(); err != nil {
		return err
	}

	return nil
}

// Close closes the engine.
func (e *Engine) Close() error {
	// get all the locks so queries, writes, and compactions stop before closing
	e.queryLock.Lock()
	defer e.queryLock.Unlock()
	e.metaLock.Lock()
	defer e.metaLock.Unlock()
	min, max := int64(math.MinInt64), int64(math.MaxInt64)
	e.writeLock.LockRange(min, max)
	defer e.writeLock.UnlockRange(min, max)
	e.filesLock.Lock()
	defer e.filesLock.Unlock()

	// ensure all deletes have been processed
	e.deletesPending.Wait()

	for _, df := range e.files {
		_ = df.Close()
	}
	e.files = nil
	e.currentFileID = 0
	e.collisions = nil
	return nil
}

// DataFileCount returns the number of data files in the database
func (e *Engine) DataFileCount() int {
	e.filesLock.RLock()
	defer e.filesLock.RUnlock()
	return len(e.files)
}

// SetLogOutput is a no-op.
func (e *Engine) SetLogOutput(w io.Writer) {}

// LoadMetadataIndex loads the shard metadata into memory.
func (e *Engine) LoadMetadataIndex(shard *tsdb.Shard, index *tsdb.DatabaseIndex, measurementFields map[string]*tsdb.MeasurementFields) error {
	// Load measurement metadata
	fields, err := e.readFields()
	if err != nil {
		return err
	}
	for k, mf := range fields {
		m := index.CreateMeasurementIndexIfNotExists(string(k))
		for name, _ := range mf.Fields {
			m.SetFieldName(name)
		}
		mf.Codec = tsdb.NewFieldCodec(mf.Fields)
		measurementFields[m.Name] = mf
	}

	// Load series metadata
	series, err := e.readSeries()
	if err != nil {
		return err
	}

	// Load the series into the in-memory index in sorted order to ensure
	// it's always consistent for testing purposes
	a := make([]string, 0, len(series))
	for k, _ := range series {
		a = append(a, k)
	}
	sort.Strings(a)
	for _, key := range a {
		s := series[key]
		s.InitializeShards()
		index.CreateSeriesIndexIfNotExists(tsdb.MeasurementFromSeriesKey(string(key)), s)
	}

	return nil
}

// WritePoints writes metadata and point data into the engine.
// Returns an error if new points are added to an existing key.
func (e *Engine) WritePoints(points []models.Point, measurementFieldsToSave map[string]*tsdb.MeasurementFields, seriesToCreate []*tsdb.SeriesCreate) error {
	return e.WAL.WritePoints(points, measurementFieldsToSave, seriesToCreate)
}

func (e *Engine) Write(pointsByKey map[string]Values, measurementFieldsToSave map[string]*tsdb.MeasurementFields, seriesToCreate []*tsdb.SeriesCreate) error {
	err, startTime, endTime, valuesByID := e.convertKeysAndWriteMetadata(pointsByKey, measurementFieldsToSave, seriesToCreate)
	if err != nil {
		return err
	}
	if len(valuesByID) == 0 {
		return nil
	}

	files, lockStart, lockEnd := e.filesAndLock(startTime, endTime)
	defer e.writeLock.UnlockRange(lockStart, lockEnd)

	if len(files) == 0 {
		return e.rewriteFile(nil, valuesByID)
	}

	maxTime := int64(math.MaxInt64)

	// do the file rewrites in parallel
	var mu sync.Mutex
	var writes sync.WaitGroup
	var errors []error

	// reverse through the data files and write in the data
	for i := len(files) - 1; i >= 0; i-- {
		f := files[i]
		// max times are exclusive, so add 1 to it
		fileMax := f.MaxTime() + 1
		fileMin := f.MinTime()
		// if the file is < rotate, write all data between fileMin and maxTime
		if f.size < e.RotateFileSize {
			writes.Add(1)
			go func(df *dataFile, vals map[uint64]Values) {
				if err := e.rewriteFile(df, vals); err != nil {
					mu.Lock()
					errors = append(errors, err)
					mu.Unlock()
				}
				writes.Done()
			}(f, e.filterDataBetweenTimes(valuesByID, fileMin, maxTime))
			continue
		}
		// if the file is > rotate:
		//   write all data between fileMax and maxTime into new file
		//   write all data between fileMin and fileMax into old file
		writes.Add(1)
		go func(vals map[uint64]Values) {
			if err := e.rewriteFile(nil, vals); err != nil {
				mu.Lock()
				errors = append(errors, err)
				mu.Unlock()
			}
			writes.Done()
		}(e.filterDataBetweenTimes(valuesByID, fileMax, maxTime))
		writes.Add(1)
		go func(df *dataFile, vals map[uint64]Values) {
			if err := e.rewriteFile(df, vals); err != nil {
				mu.Lock()
				errors = append(errors, err)
				mu.Unlock()
			}
			writes.Done()
		}(f, e.filterDataBetweenTimes(valuesByID, fileMin, fileMax))
		maxTime = fileMin
	}
	// for any data leftover, write into a new file since it's all older
	// than any file we currently have
	writes.Add(1)
	go func() {
		if err := e.rewriteFile(nil, valuesByID); err != nil {
			mu.Lock()
			errors = append(errors, err)
			mu.Unlock()
		}
		writes.Done()
	}()

	writes.Wait()

	if len(errors) > 0 {
		// TODO: log errors
		return errors[0]
	}

	if !e.SkipCompaction && e.shouldCompact() {
		go e.Compact(false)
	}

	return nil
}

// filesAndLock returns the data files that match the given range and
// ensures that the write lock will hold for the entire range
func (e *Engine) filesAndLock(min, max int64) (a dataFiles, lockStart, lockEnd int64) {
	for {
		a = make([]*dataFile, 0)
		files := e.copyFilesCollection()

		for _, f := range e.files {
			fmin, fmax := f.MinTime(), f.MaxTime()
			if min < fmax && fmin >= fmin {
				a = append(a, f)
			} else if max >= fmin && max < fmax {
				a = append(a, f)
			}
		}

		if len(a) > 0 {
			lockStart = a[0].MinTime()
			lockEnd = a[len(a)-1].MaxTime()
			if max > lockEnd {
				lockEnd = max
			}
		} else {
			lockStart = min
			lockEnd = max
		}

		e.writeLock.LockRange(lockStart, lockEnd)

		// it's possible for compaction to change the files collection while we
		// were waiting for a write lock on the range. Make sure the files are still the
		// same after we got the lock, otherwise try again. This shouldn't happen often.
		filesAfterLock := e.copyFilesCollection()
		if reflect.DeepEqual(files, filesAfterLock) {
			return
		}

		e.writeLock.UnlockRange(lockStart, lockEnd)
	}
}

func (e *Engine) Compact(fullCompaction bool) error {
	// we're looping here to ensure that the files we've marked to compact are
	// still there after we've obtained the write lock
	var minTime, maxTime int64
	var files dataFiles
	for {
		if fullCompaction {
			files = e.copyFilesCollection()
		} else {
			files = e.filesToCompact()
		}
		if len(files) < 2 {
			return nil
		}
		minTime = files[0].MinTime()
		maxTime = files[len(files)-1].MaxTime()

		e.writeLock.LockRange(minTime, maxTime)

		// if the files are different after obtaining the write lock, one or more
		// was rewritten. Release the lock and try again. This shouldn't happen really.
		var filesAfterLock dataFiles
		if fullCompaction {
			filesAfterLock = e.copyFilesCollection()
		} else {
			filesAfterLock = e.filesToCompact()
		}
		if !reflect.DeepEqual(files, filesAfterLock) {
			e.writeLock.UnlockRange(minTime, maxTime)
			continue
		}

		// we've got the write lock and the files are all there
		break
	}

	fmt.Println("Starting compaction with files:", len(files))
	st := time.Now()

	// mark the compaction as running
	e.filesLock.Lock()
	e.compactionRunning = true
	e.filesLock.Unlock()
	defer func() {
		//release the lock
		e.writeLock.UnlockRange(minTime, maxTime)
		e.filesLock.Lock()
		e.lastCompactionTime = time.Now()
		e.compactionRunning = false
		e.filesLock.Unlock()
	}()

	positions := make([]uint32, len(files))
	ids := make([]uint64, len(files))

	// initilaize for writing
	f, err := os.OpenFile(e.nextFileName(), os.O_CREATE|os.O_RDWR, 0666)
	if err != nil {
		return err
	}

	// write the magic number
	if _, err := f.Write(u32tob(magicNumber)); err != nil {
		f.Close()
		return err
	}
	for i, df := range files {
		ids[i] = btou64(df.mmap[4:12])
		positions[i] = 4
	}
	currentPosition := uint32(fileHeaderSize)
	newPositions := make([]uint32, 0)
	newIDs := make([]uint64, 0)
	buf := make([]byte, DefaultRotateBlockSize)
	for {
		// find the min ID so we can write it to the file
		minID := uint64(math.MaxUint64)
		for _, id := range ids {
			if minID > id {
				minID = id
			}
		}
		if minID == 0 { // we've emptied all the files
			break
		}

		newIDs = append(newIDs, minID)
		newPositions = append(newPositions, currentPosition)

		// write the blocks in order from the files with this id. as we
		// go merge blocks together from one file to another, if the right size
		var previousValues Values
		for i, id := range ids {
			if id != minID {
				continue
			}
			df := files[i]
			pos := positions[i]
			fid, _, block := df.block(pos)
			if fid != id {
				panic("not possible")
			}
			newPos := pos + uint32(blockHeaderSize+len(block))
			positions[i] = newPos

			// write the blocks out to file that are already at their size limit
			for {
				// if the next block is the same ID, we don't need to decod this one
				// so we can just write it out to the file
				nextID, _, nextBlock := df.block(newPos)
				newPos = newPos + uint32(blockHeaderSize+len(block))

				if len(previousValues) > 0 {
					previousValues = append(previousValues, previousValues.DecodeSameTypeBlock(block)...)
				} else if len(block) > DefaultRotateBlockSize {
					if _, err := f.Write(df.mmap[pos:newPos]); err != nil {
						return err
					}
					currentPosition += uint32(newPos - pos)
				} else {
					// TODO: handle decode error
					previousValues, _ = DecodeBlock(block)
				}

				// write the previous values and clear if we've hit the limit
				if len(previousValues) > DefaultMaxPointsPerBlock {
					b := previousValues.Encode(buf)
					if err := e.writeBlock(f, id, b); err != nil {
						// fail hard. If we can't write a file someone needs to get woken up
						panic(fmt.Sprintf("failure writing block: %s", err.Error()))
					}
					currentPosition += uint32(blockHeaderSize + len(b))
					previousValues = nil
				}

				// move to the next block in this file only if the id is the same
				if nextID != id {
					ids[i] = nextID
					break
				}
				positions[i] = newPos
				block = nextBlock
				newPos = newPos + uint32(blockHeaderSize+len(block))
			}
		}

		if len(previousValues) > 0 {
			b := previousValues.Encode(buf)
			if err := e.writeBlock(f, minID, b); err != nil {
				// fail hard. If we can't write a file someone needs to get woken up
				panic(fmt.Sprintf("failure writing block: %s", err.Error()))
			}
			currentPosition += uint32(blockHeaderSize + len(b))
		}
	}

	err, newDF := e.writeIndexAndGetDataFile(f, minTime, maxTime, newIDs, newPositions)
	if err != nil {
		return err
	}

	// update engine with new file pointers
	e.filesLock.Lock()
	var newFiles dataFiles
	for _, df := range e.files {
		// exclude any files that were compacted
		include := true
		for _, f := range files {
			if f == df {
				include = false
				break
			}
		}
		if include {
			newFiles = append(newFiles, df)
		}
	}
	newFiles = append(newFiles, newDF)
	sort.Sort(newFiles)
	e.files = newFiles
	e.filesLock.Unlock()

	fmt.Println("Compaction took ", time.Since(st))

	// delete the old files in a goroutine so running queries won't block the write
	// from completing
	e.deletesPending.Add(1)
	go func() {
		for _, f := range files {
			if err := f.Delete(); err != nil {
				// TODO: log this error
				fmt.Println("ERROR DELETING:", f.f.Name())
			}
		}
		e.deletesPending.Done()
	}()

	return nil
}

func (e *Engine) writeBlock(f *os.File, id uint64, block []byte) error {
	if _, err := f.Write(append(u64tob(id), u32tob(uint32(len(block)))...)); err != nil {
		return err
	}
	_, err := f.Write(block)
	return err
}

func (e *Engine) writeIndexAndGetDataFile(f *os.File, minTime, maxTime int64, ids []uint64, newPositions []uint32) (error, *dataFile) {
	// write the file index, starting with the series ids and their positions
	for i, id := range ids {
		if _, err := f.Write(u64tob(id)); err != nil {
			return err, nil
		}
		if _, err := f.Write(u32tob(newPositions[i])); err != nil {
			return err, nil
		}
	}

	// write the min time, max time
	if _, err := f.Write(append(u64tob(uint64(minTime)), u64tob(uint64(maxTime))...)); err != nil {
		return err, nil
	}

	// series count
	if _, err := f.Write(u32tob(uint32(len(ids)))); err != nil {
		return err, nil
	}

	// sync it and see4k back to the beginning to hand off to the mmap
	if err := f.Sync(); err != nil {
		return err, nil
	}
	if _, err := f.Seek(0, 0); err != nil {
		return err, nil
	}

	// now open it as a memory mapped data file
	newDF, err := NewDataFile(f)
	if err != nil {
		return err, nil
	}

	return nil, newDF
}

func (e *Engine) shouldCompact() bool {
	e.filesLock.RLock()
	running := e.compactionRunning
	since := time.Since(e.lastCompactionTime)
	e.filesLock.RUnlock()
	if running || since < e.IndexMinimumCompactionInterval {
		return false
	}
	return len(e.filesToCompact()) >= e.CompactionFileCount
}

func (e *Engine) filesToCompact() dataFiles {
	e.filesLock.RLock()
	defer e.filesLock.RUnlock()

	var a dataFiles
	for _, df := range e.files {
		if time.Since(df.modTime) > e.CompactionAge && df.size < MaxDataFileSize {
			a = append(a, df)
		} else if len(a) > 0 {
			// only compact contiguous ranges. If we hit the negative case and
			// there are files to compact, stop here
			break
		}
	}
	return a
}

func (e *Engine) convertKeysAndWriteMetadata(pointsByKey map[string]Values, measurementFieldsToSave map[string]*tsdb.MeasurementFields, seriesToCreate []*tsdb.SeriesCreate) (err error, minTime, maxTime int64, valuesByID map[uint64]Values) {
	e.metaLock.Lock()
	defer e.metaLock.Unlock()

	if err := e.writeNewFields(measurementFieldsToSave); err != nil {
		return err, 0, 0, nil
	}
	if err := e.writeNewSeries(seriesToCreate); err != nil {
		return err, 0, 0, nil
	}

	if len(pointsByKey) == 0 {
		return nil, 0, 0, nil
	}

	// read in keys and assign any that aren't defined
	b, err := e.readCompressedFile("ids")
	if err != nil {
		return err, 0, 0, nil
	}
	ids := make(map[string]uint64)
	if b != nil {
		if err := json.Unmarshal(b, &ids); err != nil {
			return err, 0, 0, nil
		}
	}

	// these are values that are newer than anything stored in the shard
	valuesByID = make(map[uint64]Values)

	idToKey := make(map[uint64]string)    // we only use this map if new ids are being created
	collisions := make(map[string]uint64) // we only use this if a collision is encountered
	newKeys := false
	// track the min and max time of values being inserted so we can lock that time range
	minTime = int64(math.MaxInt64)
	maxTime = int64(math.MinInt64)
	for k, values := range pointsByKey {
		var id uint64
		var ok bool
		if id, ok = ids[k]; !ok {
			// populate the map if we haven't already

			if len(idToKey) == 0 {
				for n, id := range ids {
					idToKey[id] = n
				}
			}

			// now see if the hash id collides with a different key
			hashID := e.HashSeriesField(k)
			existingKey, idInMap := idToKey[hashID]
			// we only care if the keys are different. if so, it's a hash collision we have to keep track of
			if idInMap && k != existingKey {
				// we have a collision, find this new key the next available id
				hashID = 0
				for {
					hashID++
					if _, ok := idToKey[hashID]; !ok {
						// next ID is available, use it
						break
					}
				}
				collisions[k] = hashID
			}

			newKeys = true
			ids[k] = hashID
			idToKey[hashID] = k
			id = hashID
		}

		if minTime > values.MinTime() {
			minTime = values.MinTime()
		}
		if maxTime < values.MaxTime() {
			maxTime = values.MaxTime()
		}

		valuesByID[id] = values
	}

	if newKeys {
		b, err := json.Marshal(ids)
		if err != nil {
			return err, 0, 0, nil
		}
		if err := e.replaceCompressedFile("ids", b); err != nil {
			return err, 0, 0, nil
		}
	}

	if len(collisions) > 0 {
		e.saveNewCollisions(collisions)
	}

	return
}

func (e *Engine) saveNewCollisions(collisions map[string]uint64) error {
	e.collisionsLock.Lock()
	defer e.collisionsLock.Unlock()

	for k, v := range collisions {
		e.collisions[k] = v
	}

	data, err := json.Marshal(e.collisions)

	if err != nil {
		return err
	}

	return e.replaceCompressedFile(CollisionsFileExtension, data)
}

func (e *Engine) readCollisions() error {
	e.collisions = make(map[string]uint64)
	data, err := e.readCompressedFile(CollisionsFileExtension)
	if err != nil {
		return err
	}

	if len(data) == 0 {
		return nil
	}

	return json.Unmarshal(data, &e.collisions)
}

// filterDataBetweenTimes will create a new map with data between
// the minTime (inclusive) and maxTime (exclusive) while removing that
// data from the passed in map. It is assume that the Values arrays
// are sorted in time ascending order
func (e *Engine) filterDataBetweenTimes(valuesByID map[uint64]Values, minTime, maxTime int64) map[uint64]Values {
	filteredValues := make(map[uint64]Values)
	for id, values := range valuesByID {
		maxIndex := len(values)
		minIndex := 0
		// find the index of the first value in the range
		for i, v := range values {
			t := v.UnixNano()
			if t >= minTime && t < maxTime {
				minIndex = i
				break
			}
		}
		// go backwards to find the index of the last value in the range
		for i := len(values) - 1; i >= 0; i-- {
			t := values[i].UnixNano()
			if t < maxTime {
				maxIndex = i + 1
				break
			}
		}

		// write into the result map and filter the passed in map
		filteredValues[id] = values[minIndex:maxIndex]

		// if we grabbed all the values, remove them from the passed in map
		if minIndex == len(values) || (minIndex == 0 && maxIndex == len(values)) {
			delete(valuesByID, id)
			continue
		}

		valuesByID[id] = values[0:minIndex]
		if maxIndex < len(values) {
			valuesByID[id] = append(valuesByID[id], values[maxIndex:]...)
		}
	}
	return filteredValues
}

// rewriteFile will read in the old data file, if provided and merge the values
// in the passed map into a new data file
func (e *Engine) rewriteFile(oldDF *dataFile, valuesByID map[uint64]Values) error {
	if len(valuesByID) == 0 {
		return nil
	}

	// we need the values in sorted order so that we can merge them into the
	// new file as we read the old file
	ids := make([]uint64, 0, len(valuesByID))
	for id, _ := range valuesByID {
		ids = append(ids, id)
	}

	minTime := int64(math.MaxInt64)
	maxTime := int64(math.MinInt64)

	// read header of ids to starting positions and times
	oldIDToPosition := make(map[uint64]uint32)
	if oldDF != nil {
		oldIDToPosition = oldDF.IDToPosition()
		minTime = oldDF.MinTime()
		maxTime = oldDF.MaxTime()
	}
	for _, v := range valuesByID {
		if minTime > v.MinTime() {
			minTime = v.MinTime()
		}
		if maxTime < v.MaxTime() {
			// add 1 ns to the time since maxTime is exclusive
			maxTime = v.MaxTime() + 1
		}
	}

	// add any ids that are in the file that aren't getting flushed here
	for id, _ := range oldIDToPosition {
		if _, ok := valuesByID[id]; !ok {
			ids = append(ids, id)
		}
	}

	// always write in order by ID
	sort.Sort(uint64slice(ids))

	// TODO: add checkpoint file that indicates if this completed or not
	f, err := os.OpenFile(e.nextFileName(), os.O_CREATE|os.O_RDWR, 0666)
	if err != nil {
		return err
	}

	// write the magic number
	if _, err := f.Write(u32tob(magicNumber)); err != nil {
		f.Close()
		return err
	}

	// now combine the old file data with the new values, keeping track of
	// their positions
	currentPosition := uint32(fileHeaderSize)
	newPositions := make([]uint32, len(ids))
	buf := make([]byte, DefaultMaxPointsPerBlock*20)
	for i, id := range ids {
		// mark the position for this ID
		newPositions[i] = currentPosition

		newVals := valuesByID[id]

		// if this id is only in the file and not in the new values, just copy over from old file
		if len(newVals) == 0 {
			fpos := oldIDToPosition[id]

			// write the blocks until we hit whatever the next id is
			for {
				fid := btou64(oldDF.mmap[fpos : fpos+8])
				if fid != id {
					break
				}
				length := btou32(oldDF.mmap[fpos+8 : fpos+12])
				if _, err := f.Write(oldDF.mmap[fpos : fpos+12+length]); err != nil {
					f.Close()
					return err
				}
				fpos += (12 + length)
				currentPosition += (12 + length)

				// make sure we're not at the end of the file
				if fpos >= oldDF.size {
					break
				}
			}

			continue
		}

		// if the values are not in the file, just write the new ones
		fpos, ok := oldIDToPosition[id]
		if !ok {
			// TODO: ensure we encode only the amount in a block
			block := newVals.Encode(buf)
			if err := e.writeBlock(f, id, block); err != nil {
				f.Close()
				return err
			}
			currentPosition += uint32(blockHeaderSize + len(block))

			continue
		}

		// it's in the file and the new values, combine them and write out
		for {
			fid, _, block := oldDF.block(fpos)
			if fid != id {
				break
			}
			fpos += uint32(blockHeaderSize + len(block))

			// determine if there's a block after this with the same id and get its time
			nextID, nextTime, _ := oldDF.block(fpos)
			hasFutureBlock := nextID == id

			nv, newBlock, err := e.DecodeAndCombine(newVals, block, buf[:0], nextTime, hasFutureBlock)
			newVals = nv
			if err != nil {
				return err
			}
			if _, err := f.Write(append(u64tob(id), u32tob(uint32(len(newBlock)))...)); err != nil {
				f.Close()
				return err
			}
			if _, err := f.Write(newBlock); err != nil {
				f.Close()
				return err
			}

			currentPosition += uint32(blockHeaderSize + len(newBlock))

			if fpos >= oldDF.indexPosition() {
				break
			}
		}

		// TODO: ensure we encode only the amount in a block, refactor this wil line 450 into func
		if len(newVals) > 0 {
			// TODO: ensure we encode only the amount in a block
			block := newVals.Encode(buf)
			if _, err := f.Write(append(u64tob(id), u32tob(uint32(len(block)))...)); err != nil {
				f.Close()
				return err
			}
			if _, err := f.Write(block); err != nil {
				f.Close()
				return err
			}
			currentPosition += uint32(blockHeaderSize + len(block))
		}
	}

	err, newDF := e.writeIndexAndGetDataFile(f, minTime, maxTime, ids, newPositions)
	if err != nil {
		f.Close()
		return err
	}

	// update the engine to point at the new dataFiles
	e.filesLock.Lock()
	var files dataFiles
	for _, df := range e.files {
		if df != oldDF {
			files = append(files, df)
		}
	}
	files = append(files, newDF)
	sort.Sort(files)
	e.files = files
	e.filesLock.Unlock()

	// remove the old data file. no need to block returning the write,
	// but we need to let any running queries finish before deleting it
	if oldDF != nil {
		e.deletesPending.Add(1)
		go func() {
			if err := oldDF.Delete(); err != nil {
				fmt.Println("ERROR DELETING FROM REWRITE:", oldDF.f.Name())
			}
			e.deletesPending.Done()
		}()
	}

	return nil
}

func (e *Engine) nextFileName() string {
	e.currentFileID++
	return filepath.Join(e.path, fmt.Sprintf("%07d.%s", e.currentFileID, Format))
}

func (e *Engine) readCompressedFile(name string) ([]byte, error) {
	f, err := os.OpenFile(filepath.Join(e.path, name), os.O_RDONLY, 0666)
	if os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	b, err := ioutil.ReadAll(f)
	if err != nil {
		return nil, err
	}

	data, err := snappy.Decode(nil, b)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (e *Engine) replaceCompressedFile(name string, data []byte) error {
	tmpName := filepath.Join(e.path, name+"tmp")
	f, err := os.OpenFile(tmpName, os.O_CREATE|os.O_RDWR, 0666)
	if err != nil {
		return err
	}
	b := snappy.Encode(nil, data)
	if _, err := f.Write(b); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Remove(name); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Rename(tmpName, filepath.Join(e.path, name))
}

// DeleteSeries deletes the series from the engine.
func (e *Engine) DeleteSeries(keys []string) error {
	return nil
}

// DeleteMeasurement deletes a measurement and all related series.
func (e *Engine) DeleteMeasurement(name string, seriesKeys []string) error {
	return nil
}

// SeriesCount returns the number of series buckets on the shard.
func (e *Engine) SeriesCount() (n int, err error) {
	return 0, nil
}

// Begin starts a new transaction on the engine.
func (e *Engine) Begin(writable bool) (tsdb.Tx, error) {
	e.queryLock.RLock()

	var files dataFiles

	// we do this to ensure that the data files haven't been deleted from a compaction
	// while we were waiting to get the query lock
	for {
		files = e.copyFilesCollection()

		// get the query lock
		for _, f := range files {
			f.mu.RLock()
		}

		// ensure they're all still open
		reset := false
		for _, f := range files {
			if f.f == nil {
				reset = true
				break
			}
		}

		// if not, release and try again
		if reset {
			for _, f := range files {
				f.mu.RUnlock()
			}
			continue
		}

		// we're good to go
		break
	}

	return &tx{files: files, engine: e}, nil
}

func (e *Engine) WriteTo(w io.Writer) (n int64, err error) { panic("not implemented") }

func (e *Engine) keyAndFieldToID(series, field string) uint64 {
	// get the ID for the key and be sure to check if it had hash collision before
	key := seriesFieldKey(series, field)
	e.collisionsLock.RLock()
	id, ok := e.collisions[key]
	e.collisionsLock.RUnlock()

	if !ok {
		id = e.HashSeriesField(key)
	}
	return id
}

func (e *Engine) copyFilesCollection() []*dataFile {
	e.filesLock.RLock()
	defer e.filesLock.RUnlock()
	a := make([]*dataFile, len(e.files))
	copy(a, e.files)
	return a
}

func (e *Engine) writeNewFields(measurementFieldsToSave map[string]*tsdb.MeasurementFields) error {
	if len(measurementFieldsToSave) == 0 {
		return nil
	}

	// read in all the previously saved fields
	fields, err := e.readFields()
	if err != nil {
		return err
	}

	// add the new ones or overwrite old ones
	for name, mf := range measurementFieldsToSave {
		fields[name] = mf
	}

	return e.writeFields(fields)
}

func (e *Engine) writeFields(fields map[string]*tsdb.MeasurementFields) error {
	// compress and save everything
	data, err := json.Marshal(fields)
	if err != nil {
		return err
	}

	fn := filepath.Join(e.path, FieldsFileExtension+"tmp")
	ff, err := os.OpenFile(fn, os.O_CREATE|os.O_RDWR, 0666)
	if err != nil {
		return err
	}
	_, err = ff.Write(snappy.Encode(nil, data))
	if err != nil {
		return err
	}
	if err := ff.Close(); err != nil {
		return err
	}
	fieldsFileName := filepath.Join(e.path, FieldsFileExtension)

	if _, err := os.Stat(fieldsFileName); !os.IsNotExist(err) {
		if err := os.Remove(fieldsFileName); err != nil {
			return err
		}
	}

	return os.Rename(fn, fieldsFileName)
}

func (e *Engine) readFields() (map[string]*tsdb.MeasurementFields, error) {
	fields := make(map[string]*tsdb.MeasurementFields)

	f, err := os.OpenFile(filepath.Join(e.path, FieldsFileExtension), os.O_RDONLY, 0666)
	if os.IsNotExist(err) {
		return fields, nil
	} else if err != nil {
		return nil, err
	}
	b, err := ioutil.ReadAll(f)
	if err != nil {
		return nil, err
	}

	data, err := snappy.Decode(nil, b)
	if err != nil {
		return nil, err
	}

	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}

	return fields, nil
}

func (e *Engine) writeNewSeries(seriesToCreate []*tsdb.SeriesCreate) error {
	if len(seriesToCreate) == 0 {
		return nil
	}

	// read in previously saved series
	series, err := e.readSeries()
	if err != nil {
		return err
	}

	// add new ones, compress and save
	for _, s := range seriesToCreate {
		series[s.Series.Key] = s.Series
	}

	return e.writeSeries(series)
}

func (e *Engine) writeSeries(series map[string]*tsdb.Series) error {
	data, err := json.Marshal(series)
	if err != nil {
		return err
	}

	fn := filepath.Join(e.path, SeriesFileExtension+"tmp")
	ff, err := os.OpenFile(fn, os.O_CREATE|os.O_RDWR, 0666)
	if err != nil {
		return err
	}
	_, err = ff.Write(snappy.Encode(nil, data))
	if err != nil {
		return err
	}
	if err := ff.Close(); err != nil {
		return err
	}
	seriesFileName := filepath.Join(e.path, SeriesFileExtension)

	if _, err := os.Stat(seriesFileName); !os.IsNotExist(err) {
		if err := os.Remove(seriesFileName); err != nil && err != os.ErrNotExist {
			return err
		}
	}

	return os.Rename(fn, seriesFileName)
}

func (e *Engine) readSeries() (map[string]*tsdb.Series, error) {
	series := make(map[string]*tsdb.Series)

	f, err := os.OpenFile(filepath.Join(e.path, SeriesFileExtension), os.O_RDONLY, 0666)
	if os.IsNotExist(err) {
		return series, nil
	} else if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := ioutil.ReadAll(f)
	if err != nil {
		return nil, err
	}

	data, err := snappy.Decode(nil, b)
	if err != nil {
		return nil, err
	}

	if err := json.Unmarshal(data, &series); err != nil {
		return nil, err
	}

	return series, nil
}

// DecodeAndCombine take an encoded block from a file, decodes it and interleaves the file
// values with the values passed in. nextTime and hasNext refer to if the file
// has future encoded blocks so that this method can know how much of its values can be
// combined and output in the resulting encoded block.
func (e *Engine) DecodeAndCombine(newValues Values, block, buf []byte, nextTime int64, hasFutureBlock bool) (Values, []byte, error) {
	values := newValues.DecodeSameTypeBlock(block)

	var remainingValues Values

	if hasFutureBlock {
		// take all values that have times less than the future block and update the vals array
		pos := sort.Search(len(newValues), func(i int) bool {
			return newValues[i].Time().UnixNano() >= nextTime
		})
		values = append(values, newValues[:pos]...)
		remainingValues = newValues[pos:]
		values = values.Deduplicate()
	} else {
		requireSort := values.MaxTime() >= newValues.MinTime()
		values = append(values, newValues...)
		if requireSort {
			values = values.Deduplicate()
		}
	}

	if len(values) > DefaultMaxPointsPerBlock {
		remainingValues = values[DefaultMaxPointsPerBlock:]
		values = values[:DefaultMaxPointsPerBlock]
	}

	return remainingValues, values.Encode(buf), nil
}

type dataFile struct {
	f       *os.File
	mu      sync.RWMutex
	size    uint32
	modTime time.Time
	mmap    []byte
}

// byte size constants for the data file
const (
	fileHeaderSize     = 4
	seriesCountSize    = 4
	timeSize           = 8
	blockHeaderSize    = 12
	seriesIDSize       = 8
	seriesPositionSize = 4
	seriesHeaderSize   = seriesIDSize + seriesPositionSize
)

func NewDataFile(f *os.File) (*dataFile, error) {
	fInfo, err := f.Stat()
	if err != nil {
		return nil, err
	}
	mmap, err := syscall.Mmap(int(f.Fd()), 0, int(fInfo.Size()), syscall.PROT_READ, syscall.MAP_SHARED|MAP_POPULATE)
	if err != nil {
		return nil, err
	}

	return &dataFile{
		f:       f,
		mmap:    mmap,
		size:    uint32(fInfo.Size()),
		modTime: fInfo.ModTime(),
	}, nil
}

func (d *dataFile) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.close()
}

func (d *dataFile) Delete() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.close(); err != nil {
		return err
	}
	err := os.Remove(d.f.Name())
	if err != nil {
		return err
	}
	d.f = nil
	return nil
}

func (d *dataFile) close() error {
	if d.mmap == nil {
		return nil
	}
	err := syscall.Munmap(d.mmap)
	if err != nil {
		return err
	}

	d.mmap = nil
	return d.f.Close()
}

func (d *dataFile) MinTime() int64 {
	return int64(btou64(d.mmap[d.size-20 : d.size-12]))
}

func (d *dataFile) MaxTime() int64 {
	return int64(btou64(d.mmap[d.size-12 : d.size-4]))
}

func (d *dataFile) SeriesCount() uint32 {
	return btou32(d.mmap[d.size-4:])
}

func (d *dataFile) IDToPosition() map[uint64]uint32 {
	count := int(d.SeriesCount())
	m := make(map[uint64]uint32)

	indexStart := d.size - uint32(count*12+20)
	for i := 0; i < count; i++ {
		offset := indexStart + uint32(i*12)
		id := btou64(d.mmap[offset : offset+8])
		pos := btou32(d.mmap[offset+8 : offset+12])
		m[id] = pos
	}

	return m
}

func (d *dataFile) indexPosition() uint32 {
	return d.size - uint32(d.SeriesCount()*12+20)
}

// StartingPositionForID returns the position in the file of the
// first block for the given ID. If zero is returned the ID doesn't
// have any data in this file.
func (d *dataFile) StartingPositionForID(id uint64) uint32 {

	seriesCount := d.SeriesCount()
	indexStart := d.indexPosition()

	min := uint32(0)
	max := uint32(seriesCount)

	for min < max {
		mid := (max-min)/2 + min

		offset := mid*seriesHeaderSize + indexStart
		checkID := btou64(d.mmap[offset : offset+8])

		if checkID == id {
			return btou32(d.mmap[offset+8 : offset+12])
		} else if checkID < id {
			min = mid + 1
		} else {
			max = mid
		}
	}

	return uint32(0)
}

func (d *dataFile) block(pos uint32) (id uint64, t int64, block []byte) {
	if pos < d.indexPosition() {
		id = btou64(d.mmap[pos : pos+8])
		length := btou32(d.mmap[pos+8 : pos+12])
		block = d.mmap[pos+blockHeaderSize : pos+blockHeaderSize+length]
		t = int64(btou64(d.mmap[pos+blockHeaderSize : pos+blockHeaderSize+8]))
	}
	return
}

type dataFiles []*dataFile

func (a dataFiles) Len() int           { return len(a) }
func (a dataFiles) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a dataFiles) Less(i, j int) bool { return a[i].MinTime() < a[j].MinTime() }

type cursor struct {
	id       uint64
	f        *dataFile
	filesPos int // the index in the files slice we're looking at
	pos      uint32
	vals     Values

	ascending bool

	// time acending list of data files
	files []*dataFile
}

func newCursor(id uint64, files []*dataFile, ascending bool) *cursor {
	return &cursor{
		id:        id,
		ascending: ascending,
		files:     files,
	}
}

func (c *cursor) SeekTo(seek int64) (int64, interface{}) {
	if len(c.files) == 0 {
		return tsdb.EOF, nil
	}

	if seek < c.files[0].MinTime() {
		c.filesPos = 0
		c.f = c.files[0]
	} else {
		for i, f := range c.files {
			if seek >= f.MinTime() && seek <= f.MaxTime() {
				c.filesPos = i
				c.f = f
				break
			}
		}
	}

	if c.f == nil {
		return tsdb.EOF, nil
	}

	// TODO: make this for the reverse direction cursor

	// now find the spot in the file we need to go
	for {
		pos := c.f.StartingPositionForID(c.id)

		// if this id isn't in this file, move to next one or return
		if pos == 0 {
			c.filesPos++
			if c.filesPos >= len(c.files) {
				return tsdb.EOF, nil
			}
			c.f = c.files[c.filesPos]
			continue
		}

		// seek to the block and values we're looking for
		for {
			// if the time is between this block and the next,
			// decode this block and go, otherwise seek to next block
			length := btou32(c.f.mmap[pos+8 : pos+12])

			// if the next block has a time less than what we're seeking to,
			// skip decoding this block and continue on
			nextBlockPos := pos + 12 + length
			if nextBlockPos < c.f.size {
				nextBlockID := btou64(c.f.mmap[nextBlockPos : nextBlockPos+8])
				if nextBlockID == c.id {
					nextBlockTime := int64(btou64(c.f.mmap[nextBlockPos+12 : nextBlockPos+20]))
					if nextBlockTime <= seek {
						pos = nextBlockPos
						continue
					}
				}
			}

			// it must be in this block or not at all
			t, v := c.decodeBlockAndGetValues(pos)
			if t >= seek {
				return t, v
			}

			// wasn't in the first value popped out of the block, check the rest
			for i, v := range c.vals {
				if v.Time().UnixNano() >= seek {
					c.vals = c.vals[i+1:]
					return v.Time().UnixNano(), v.Value()
				}
			}

			// not in this one, let the top loop look for it in the next file
			break
		}
	}
}

func (c *cursor) Next() (int64, interface{}) {
	if len(c.vals) == 0 {
		// if we have a file set, see if the next block is for this ID
		if c.f != nil && c.pos < c.f.size {
			nextBlockID := btou64(c.f.mmap[c.pos : c.pos+8])
			if nextBlockID == c.id && c.pos != c.f.indexPosition() {
				return c.decodeBlockAndGetValues(c.pos)
			}
		}

		// if the file is nil we hit the end of the previous file, advance the file cursor
		if c.f != nil {
			c.filesPos++
		}

		// loop until we find a file with some data
		for c.filesPos < len(c.files) {
			f := c.files[c.filesPos]

			startingPos := f.StartingPositionForID(c.id)
			if startingPos == 0 {
				c.filesPos++
				continue
			}
			c.f = f
			return c.decodeBlockAndGetValues(startingPos)
		}

		// we didn't get to a file that had a next value
		return tsdb.EOF, nil
	}

	v := c.vals[0]
	c.vals = c.vals[1:]

	return v.Time().UnixNano(), v.Value()
}

func (c *cursor) decodeBlockAndGetValues(position uint32) (int64, interface{}) {
	length := btou32(c.f.mmap[position+8 : position+12])
	block := c.f.mmap[position+12 : position+12+length]
	c.vals, _ = DecodeBlock(block)
	c.pos = position + 12 + length

	v := c.vals[0]
	c.vals = c.vals[1:]

	return v.Time().UnixNano(), v.Value()
}

func (c *cursor) Ascending() bool { return c.ascending }

// u64tob converts a uint64 into an 8-byte slice.
func u64tob(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}

func btou64(b []byte) uint64 {
	return binary.BigEndian.Uint64(b)
}

func u32tob(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

func btou32(b []byte) uint32 {
	return uint32(binary.BigEndian.Uint32(b))
}

func hashSeriesField(key string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(key))
	return h.Sum64()
}

// seriesFieldKey combine a series key and field name for a unique string to be hashed to a numeric ID
func seriesFieldKey(seriesKey, field string) string {
	return seriesKey + "#" + field
}

type uint64slice []uint64

func (a uint64slice) Len() int           { return len(a) }
func (a uint64slice) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a uint64slice) Less(i, j int) bool { return a[i] < a[j] }