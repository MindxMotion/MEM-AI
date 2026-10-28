package model

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// Error diagnostics are best effort, unlike quota and security audit records.
type diagnosticWriter[T any] struct {
	queue     chan T
	name      string
	maintain  func(*gorm.DB) error
	done      chan struct{}
	cancel    context.CancelFunc
	mu        sync.RWMutex
	closed    bool
	accepted  atomic.Uint64
	written   atomic.Uint64
	dropped   atomic.Uint64
	failed    atomic.Uint64
	startOnce sync.Once
}

type errorLogWriter = diagnosticWriter[Log]

var relayErrorLogs = newErrorLogWriter(1024)

func newErrorLogWriter(capacity int) *errorLogWriter {
	return newDiagnosticWriter[Log](capacity, "async relay error logs")
}

func newDiagnosticWriter[T any](capacity int, name string) *diagnosticWriter[T] {
	return &diagnosticWriter[T]{queue: make(chan T, capacity), done: make(chan struct{}), name: name}
}

func (w *diagnosticWriter[T]) enqueue(log T) bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if !w.closed {
		select {
		case w.queue <- log:
			w.accepted.Add(1)
			return true
		default:
		}
	}
	w.dropped.Add(1)
	return false
}

func (w *diagnosticWriter[T]) start(database *gorm.DB) {
	w.startOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		w.cancel = cancel
		go func() {
			defer cancel()
			w.run(ctx, database)
		}()
	})
}

func (w *diagnosticWriter[T]) run(ctx context.Context, database *gorm.DB) {
	defer close(w.done)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	var reported [4]uint64
	lastMaintenance := time.Now()
	report := func() {
		counts := [4]uint64{w.accepted.Load(), w.written.Load(), w.dropped.Load(), w.failed.Load()}
		if counts == reported {
			return
		}
		message := common.LogText("async relay error logs: accepted=%d written=%d dropped=%d failed=%d queued=%d", counts[0], counts[1], counts[2], counts[3], len(w.queue))
		if w.name == "async relay outcomes" {
			message = common.LogText("async relay outcomes: accepted=%d written=%d dropped=%d failed=%d queued=%d", counts[0], counts[1], counts[2], counts[3], len(w.queue))
		}
		if counts[2] > reported[2] || counts[3] > reported[3] {
			common.SysError(message)
		} else {
			common.SysLog(message)
		}
		reported = counts
	}
	defer report()
	for {
		select {
		case log, ok := <-w.queue:
			if !ok {
				return
			}
			writeContext, cancel := context.WithTimeout(ctx, 3*time.Second)
			err := database.Session(&gorm.Session{Logger: database.Logger.LogMode(gormlogger.Silent)}).WithContext(writeContext).Create(&log).Error
			cancel()
			if err != nil {
				w.failed.Add(1)
			} else {
				w.written.Add(1)
			}
		case <-ticker.C:
			report()
			if w.maintain != nil && time.Since(lastMaintenance) >= time.Hour {
				writeContext, cancel := context.WithTimeout(ctx, 3*time.Second)
				err := w.maintain(database.WithContext(writeContext))
				cancel()
				if err != nil {
					common.SysError(common.LogText("relay outcome retention cleanup failed"))
				}
				lastMaintenance = time.Now()
			}
		}
	}
}

func (w *diagnosticWriter[T]) stop(ctx context.Context) error {
	w.mu.Lock()
	if !w.closed {
		w.closed = true
		close(w.queue)
	}
	w.mu.Unlock()
	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		if w.cancel != nil {
			w.cancel()
		}
		return ctx.Err()
	}
}

func StartErrorLogWriter() {
	select {
	case <-relayErrorLogs.done:
		relayErrorLogs = newErrorLogWriter(1024)
	default:
	}
	relayErrorLogs.start(LOG_DB)
	common.SysLog(common.LogText("async relay error logging enabled: capacity=1024 workers=1 write_timeout=3s"))
}

func StopErrorLogWriter(ctx context.Context) error {
	return relayErrorLogs.stop(ctx)
}
