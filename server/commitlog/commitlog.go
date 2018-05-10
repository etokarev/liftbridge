package commitlog

import (
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	atomic_file "github.com/natefinch/atomic"
	"github.com/pkg/errors"
)

var (
	ErrSegmentNotFound = errors.New("segment not found")
)

const (
	LogFileSuffix               = ".log"
	IndexFileSuffix             = ".index"
	hwFileName                  = "replication-offset-checkpoint"
	defaultMaxSegmentBytes      = 10485760
	defaultHWCheckpointInterval = 5 * time.Second
)

type CommitLog struct {
	Options
	cleaner        Cleaner
	name           string
	mu             sync.RWMutex
	hw             int64
	flushHW        chan struct{}
	segments       []*Segment
	vActiveSegment atomic.Value
	waitersMu      sync.Mutex
	waiters        map[*Reader]chan struct{}
}

type Options struct {
	Path string
	// MaxSegmentBytes is the max number of bytes a segment can contain, once the limit is hit a
	// new segment will be split off.
	MaxSegmentBytes      int64
	MaxLogBytes          int64
	Compact              bool
	hwCheckpointInterval time.Duration
}

func New(opts Options) (*CommitLog, error) {
	if opts.Path == "" {
		return nil, errors.New("path is empty")
	}

	if opts.MaxSegmentBytes == 0 {
		opts.MaxSegmentBytes = defaultMaxSegmentBytes
	}
	if opts.hwCheckpointInterval == 0 {
		opts.hwCheckpointInterval = defaultHWCheckpointInterval
	}

	var cleaner Cleaner
	if opts.Compact {
		cleaner = NewCompactCleaner()
	} else {
		cleaner = NewDeleteCleaner(opts.MaxLogBytes)
	}

	path, _ := filepath.Abs(opts.Path)
	l := &CommitLog{
		Options: opts,
		name:    filepath.Base(path),
		cleaner: cleaner,
		waiters: make(map[*Reader]chan struct{}),
		hw:      -1,
		flushHW: make(chan struct{}),
	}

	if err := l.init(); err != nil {
		return nil, err
	}

	if err := l.open(); err != nil {
		return nil, err
	}

	go l.checkpointHW()

	return l, nil
}

func (l *CommitLog) init() error {
	err := os.MkdirAll(l.Path, 0755)
	if err != nil {
		return errors.Wrap(err, "mkdir failed")
	}
	return nil
}

func (l *CommitLog) open() error {
	files, err := ioutil.ReadDir(l.Path)
	if err != nil {
		return errors.Wrap(err, "read dir failed")
	}
	for _, file := range files {
		// if this file is an index file, make sure it has a corresponding .log file
		if strings.HasSuffix(file.Name(), IndexFileSuffix) {
			_, err := os.Stat(filepath.Join(l.Path, strings.Replace(file.Name(), IndexFileSuffix, LogFileSuffix, 1)))
			if os.IsNotExist(err) {
				if err := os.Remove(file.Name()); err != nil {
					return err
				}
			} else if err != nil {
				return errors.Wrap(err, "stat file failed")
			}
		} else if strings.HasSuffix(file.Name(), LogFileSuffix) {
			offsetStr := strings.TrimSuffix(file.Name(), LogFileSuffix)
			baseOffset, err := strconv.Atoi(offsetStr)
			if err != nil {
				return err
			}
			segment, err := NewSegment(l.Path, int64(baseOffset), l.MaxSegmentBytes)
			if err != nil {
				return err
			}
			l.segments = append(l.segments, segment)
		} else if file.Name() == hwFileName {
			// Recover high watermark.
			b, err := ioutil.ReadFile(filepath.Join(l.Path, file.Name()))
			if err != nil {
				return errors.Wrap(err, "read high watermark file failed")
			}
			hw, err := strconv.ParseInt(string(b), 10, 64)
			if err != nil {
				return errors.Wrap(err, "parse high watermark file failed")
			}
			l.hw = hw
		}
	}
	if len(l.segments) == 0 {
		segment, err := NewSegment(l.Path, 0, l.MaxSegmentBytes)
		if err != nil {
			return err
		}
		l.segments = append(l.segments, segment)
	}
	l.vActiveSegment.Store(l.segments[len(l.segments)-1])
	return nil
}

func (l *CommitLog) Append(b []byte) (offset int64, err error) {
	ms := MessageSet(b)
	if l.checkSplit() {
		if err := l.split(); err != nil {
			return offset, err
		}
	}
	position := l.activeSegment().Position
	offset = l.activeSegment().NextOffset
	ms.PutOffset(offset)
	if _, err := l.activeSegment().Write(ms); err != nil {
		return offset, err
	}
	e := Entry{
		Offset:   offset,
		Position: position,
	}
	if err := l.activeSegment().Index.WriteEntry(e); err != nil {
		return offset, err
	}
	l.notifyWaiters()
	return offset, nil
}

func (l *CommitLog) notifyWaiters() {
	l.waitersMu.Lock()
	for r, ch := range l.waiters {
		close(ch)
		delete(l.waiters, r)
	}
	l.waitersMu.Unlock()
}

func (l *CommitLog) Read(p []byte) (n int, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.activeSegment().Read(p)
}

func (l *CommitLog) NewestOffset() int64 {
	return l.activeSegment().NextOffset - 1
}

func (l *CommitLog) OldestOffset() int64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.segments[0].BaseOffset
}

func (l *CommitLog) SetHighWatermark(hw int64) {
	l.mu.Lock()
	l.hw = hw
	l.mu.Unlock()
	// TODO: should we flush the HW to disk here?
}

func (l *CommitLog) HighWatermark() int64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.hw
}

func (l *CommitLog) activeSegment() *Segment {
	return l.vActiveSegment.Load().(*Segment)
}

func (l *CommitLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, segment := range l.segments {
		if err := segment.Close(); err != nil {
			return err
		}
	}
	l.flushHW <- struct{}{}
	close(l.flushHW)
	return nil
}

func (l *CommitLog) Delete() error {
	if err := l.Close(); err != nil {
		return err
	}
	return os.RemoveAll(l.Path)
}

func (l *CommitLog) Truncate(offset int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	segment, idx := findSegment(l.segments, offset)
	if segment == nil {
		// Nothing to truncate.
		return nil
	}

	// Delete all following segments.
	deleted := 0
	for i := idx + 1; i < len(l.segments); i++ {
		if err := l.segments[i].Delete(); err != nil {
			return err
		}
		deleted++
	}

	// Delete the segment if its base offset is the target offset.
	if segment.BaseOffset == offset {
		if err := segment.Delete(); err != nil {
			return err
		}
		deleted++
	}

	// Retain all preceding segments.
	segments := make([]*Segment, len(l.segments)-deleted)
	for i := 0; i < idx; i++ {
		segments[i] = l.segments[i]
	}

	// Replace segment containing offset with truncated segment.
	if segment.BaseOffset != offset {
		var (
			ss              = NewSegmentScanner(segment)
			newSegment, err = NewSegment(
				segment.path, segment.BaseOffset,
				segment.maxBytes, truncatedSuffix)
		)
		if err != nil {
			return err
		}
		for ms, err := ss.Scan(); err == nil; ms, err = ss.Scan() {
			if ms.Offset() < offset {
				if _, err = newSegment.Write(ms); err != nil {
					return err
				}
			} else {
				break
			}
		}
		if err = newSegment.Replace(segment); err != nil {
			return err
		}
		segments[idx] = newSegment
	}
	l.segments = segments
	return nil
}

func (l *CommitLog) Segments() []*Segment {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.segments
}

func (l *CommitLog) checkSplit() bool {
	return l.activeSegment().IsFull()
}

func (l *CommitLog) split() error {
	segment, err := NewSegment(l.Path, l.NewestOffset()+1, l.MaxSegmentBytes)
	if err != nil {
		return err
	}
	l.mu.Lock()
	segments := append(l.segments, segment)
	segments, err = l.cleaner.Clean(segments)
	if err != nil {
		l.mu.Unlock()
		return err
	}
	l.segments = segments
	l.mu.Unlock()
	l.vActiveSegment.Store(segment)
	return nil
}

func (l *CommitLog) checkpointHW() {
	var (
		ticker = time.NewTicker(l.hwCheckpointInterval)
		file   = filepath.Join(l.Path, hwFileName)
	)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
		case _, ok := <-l.flushHW:
			if !ok {
				return
			}
		}
		var (
			hw = l.HighWatermark()
			r  = strings.NewReader(strconv.FormatInt(hw, 10))
		)
		if err := atomic_file.WriteFile(file, r); err != nil {
			panic(errors.Wrap(err, "failed to checkpoint high watermark"))
		}
	}
}
