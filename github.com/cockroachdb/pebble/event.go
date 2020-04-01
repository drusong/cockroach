// Copyright 2018 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package pebble

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"github.com/cockroachdb/pebble/internal/humanize"
	"github.com/cockroachdb/pebble/internal/manifest"
)

// TableInfo exports the manifest.TableInfo type.
type TableInfo = manifest.TableInfo

func tablesTotalSize(tables []TableInfo) uint64 {
	var size uint64
	for i := range tables {
		size += tables[i].Size
	}
	return size
}

func formatFileNums(tables []TableInfo) string {
	var buf strings.Builder
	for i := range tables {
		if i > 0 {
			buf.WriteString(" ")
		}
		buf.WriteString(tables[i].FileNum.String())
	}
	return buf.String()
}

// CompactionInfo contains the info for a compaction event.
type CompactionInfo struct {
	// JobID is the ID of the compaction job.
	JobID int
	// Reason is the reason for the compaction.
	Reason string
	// Input contains the input tables for the compaction. A compaction is
	// performed from Input.Level to Input.Level+1. Input.Tables[0] contains the
	// inputs from Input.Level and Input.Tables[1] contains the inputs from
	// Input.Level+1.
	Input struct {
		Level  int
		Tables [2][]TableInfo
	}
	// Output contains the output tables generated by the compaction. The output
	// tables are empty for the compaction begin event.
	Output struct {
		Level  int
		Tables []TableInfo
	}
	Duration time.Duration
	Done     bool
	Err      error
}

func (i CompactionInfo) String() string {
	if i.Err != nil {
		return fmt.Sprintf("[JOB %d] compaction to L%d error: %s",
			i.JobID, i.Output.Level, i.Err)
	}

	if !i.Done {
		return fmt.Sprintf("[JOB %d] compacting L%d [%s] (%s) + L%d [%s] (%s)",
			i.JobID,
			i.Input.Level,
			formatFileNums(i.Input.Tables[0]),
			humanize.Uint64(tablesTotalSize(i.Input.Tables[0])),
			i.Output.Level,
			formatFileNums(i.Input.Tables[1]),
			humanize.Uint64(tablesTotalSize(i.Input.Tables[1])))
	}

	outputSize := tablesTotalSize(i.Output.Tables)
	return fmt.Sprintf("[JOB %d] compacted L%d [%s] (%s) + L%d [%s] (%s) -> L%d [%s] (%s), in %.1fs, output rate %s/s",
		i.JobID,
		i.Input.Level,
		formatFileNums(i.Input.Tables[0]),
		humanize.Uint64(tablesTotalSize(i.Input.Tables[0])),
		i.Output.Level,
		formatFileNums(i.Input.Tables[1]),
		humanize.Uint64(tablesTotalSize(i.Input.Tables[1])),
		i.Output.Level,
		formatFileNums(i.Output.Tables),
		humanize.Uint64(outputSize),
		i.Duration.Seconds(),
		humanize.Uint64(uint64(float64(outputSize)/i.Duration.Seconds())))
}

// FlushInfo contains the info for a flush event.
type FlushInfo struct {
	// JobID is the ID of the flush job.
	JobID int
	// Reason is the reason for the flush.
	Reason string
	// Output contains the ouptut table generated by the flush. The output info
	// is empty for the flush begin event.
	Output []TableInfo
	Done   bool
	Err    error
}

func (i FlushInfo) String() string {
	if i.Err != nil {
		return fmt.Sprintf("[JOB %d] flush error: %s", i.JobID, i.Err)
	}

	if !i.Done {
		return fmt.Sprintf("[JOB %d] flushing to L0", i.JobID)
	}

	return fmt.Sprintf("[JOB %d] flushed to L0 [%s] (%s)",
		i.JobID,
		formatFileNums(i.Output),
		humanize.Uint64(tablesTotalSize(i.Output)))
}

// ManifestCreateInfo contains info about a manifest creation event.
type ManifestCreateInfo struct {
	// JobID is the ID of the job the caused the manifest to be created.
	JobID int
	Path  string
	// The file number of the new Manifest.
	FileNum FileNum
	Err     error
}

func (i ManifestCreateInfo) String() string {
	if i.Err != nil {
		return fmt.Sprintf("[JOB %d] MANIFEST create error: %s", i.JobID, i.Err)
	}
	return fmt.Sprintf("[JOB %d] MANIFEST created %s", i.JobID, i.FileNum)
}

// ManifestDeleteInfo contains the info for a Manifest deletion event.
type ManifestDeleteInfo struct {
	// JobID is the ID of the job the caused the Manifest to be deleted.
	JobID   int
	Path    string
	FileNum FileNum
	Err     error
}

func (i ManifestDeleteInfo) String() string {
	if i.Err != nil {
		return fmt.Sprintf("[JOB %d] MANIFEST delete error: %s", i.JobID, i.Err)
	}
	return fmt.Sprintf("[JOB %d] MANIFEST deleted %s", i.JobID, i.FileNum)
}

// TableCreateInfo contains the info for a table creation event.
type TableCreateInfo struct {
	JobID int
	// Reason is the reason for the table creation: "compacting", "flushing", or
	// "ingesting".
	Reason  string
	Path    string
	FileNum FileNum
}

func (i TableCreateInfo) String() string {
	return fmt.Sprintf("[JOB %d] %s: sstable created %s", i.JobID, i.Reason, i.FileNum)
}

// TableDeleteInfo contains the info for a table deletion event.
type TableDeleteInfo struct {
	JobID   int
	Path    string
	FileNum FileNum
	Err     error
}

func (i TableDeleteInfo) String() string {
	if i.Err != nil {
		return fmt.Sprintf("[JOB %d] sstable delete error %s: %s",
			i.JobID, i.FileNum, i.Err)
	}
	return fmt.Sprintf("[JOB %d] sstable deleted %s", i.JobID, i.FileNum)
}

// TableIngestInfo contains the info for a table ingestion event.
type TableIngestInfo struct {
	// JobID is the ID of the job the caused the table to be ingested.
	JobID  int
	Tables []struct {
		TableInfo
		Level int
	}
	// GlobalSeqNum is the sequence number that was assigned to all entries in
	// the ingested table.
	GlobalSeqNum uint64
	Err          error
}

func (i TableIngestInfo) String() string {
	if i.Err != nil {
		return fmt.Sprintf("[JOB %d] ingest error: %s", i.JobID, i.Err)
	}

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "[JOB %d] ingested", i.JobID)
	for j := range i.Tables {
		t := &i.Tables[j]
		if j > 0 {
			fmt.Fprintf(&buf, ",")
		}
		fmt.Fprintf(&buf, " L%d:%s (%s)", t.Level, t.FileNum, humanize.Uint64(t.Size))
	}
	return buf.String()
}

// WALCreateInfo contains info about a WAL creation event.
type WALCreateInfo struct {
	// JobID is the ID of the job the caused the WAL to be created.
	JobID int
	Path  string
	// The file number of the new WAL.
	FileNum FileNum
	// The file number of a previous WAL which was recycled to create this
	// one. Zero if recycling did not take place.
	RecycledFileNum FileNum
	Err             error
}

func (i WALCreateInfo) String() string {
	if i.Err != nil {
		return fmt.Sprintf("[JOB %d] WAL create error: %s", i.JobID, i.Err)
	}

	if i.RecycledFileNum == 0 {
		return fmt.Sprintf("[JOB %d] WAL created %s", i.JobID, i.FileNum)
	}

	return fmt.Sprintf("[JOB %d] WAL created %s (recycled %s)",
		i.JobID, i.FileNum, i.RecycledFileNum)
}

// WALDeleteInfo contains the info for a WAL deletion event.
type WALDeleteInfo struct {
	// JobID is the ID of the job the caused the WAL to be deleted.
	JobID   int
	Path    string
	FileNum FileNum
	Err     error
}

func (i WALDeleteInfo) String() string {
	if i.Err != nil {
		return fmt.Sprintf("[JOB %d] WAL delete error: %s", i.JobID, i.Err)
	}
	return fmt.Sprintf("[JOB %d] WAL deleted %s", i.JobID, i.FileNum)
}

// WriteStallBeginInfo contains the info for a write stall begin event.
type WriteStallBeginInfo struct {
	Reason string
}

func (i WriteStallBeginInfo) String() string {
	return fmt.Sprintf("write stall beginning: %s", i.Reason)
}

// EventListener contains a set of functions that will be invoked when various
// significant DB events occur. Note that the functions should not run for an
// excessive amount of time as they are invoked synchronously by the DB and may
// block continued DB work. For a similar reason it is advisable to not perform
// any synchronous calls back into the DB.
type EventListener struct {
	// BackgroundError is invoked whenever an error occurs during a background
	// operation such as flush or compaction.
	BackgroundError func(error)

	// CompactionBegin is invoked after the inputs to a compaction have been
	// determined, but before the compaction has produced any output.
	CompactionBegin func(CompactionInfo)

	// CompactionEnd is invoked after a compaction has completed and the result
	// has been installed.
	CompactionEnd func(CompactionInfo)

	// FlushBegin is invoked after the inputs to a flush have been determined,
	// but before the flush has produced any output.
	FlushBegin func(FlushInfo)

	// FlushEnd is invoked after a flush has complated and the result has been
	// installed.
	FlushEnd func(FlushInfo)

	// ManifestCreated is invoked after a manifest has been created.
	ManifestCreated func(ManifestCreateInfo)

	// ManifestDeleted is invoked after a manifest has been deleted.
	ManifestDeleted func(ManifestDeleteInfo)

	// TableCreated is invoked when a table has been created.
	TableCreated func(TableCreateInfo)

	// TableDeleted is invoked after a table has been deleted.
	TableDeleted func(TableDeleteInfo)

	// TableIngested is invoked after an externally created table has been
	// ingested via a call to DB.Ingest().
	TableIngested func(TableIngestInfo)

	// WALCreated is invoked after a WAL has been created.
	WALCreated func(WALCreateInfo)

	// WALDeleted is invoked after a WAL has been deleted.
	WALDeleted func(WALDeleteInfo)

	// WriteStallBegin is invoked when writes are intentionally delayed.
	WriteStallBegin func(WriteStallBeginInfo)

	// WriteStallEnd is invoked when delayed writes are released.
	WriteStallEnd func()
}

// EnsureDefaults ensures that background error events are logged to the
// specified logger if a handler for those events hasn't been otherwise
// specified. Ensure all handlers are non-nil so that we don't have to check
// for nil-ness before invoking.
func (l *EventListener) EnsureDefaults(logger Logger) {
	if l.BackgroundError == nil {
		l.BackgroundError = func(err error) {
			logger.Infof("background error: %s", err)
		}
	}
	if l.CompactionBegin == nil {
		l.CompactionBegin = func(info CompactionInfo) {}
	}
	if l.CompactionEnd == nil {
		l.CompactionEnd = func(info CompactionInfo) {}
	}
	if l.FlushBegin == nil {
		l.FlushBegin = func(info FlushInfo) {}
	}
	if l.FlushEnd == nil {
		l.FlushEnd = func(info FlushInfo) {}
	}
	if l.ManifestCreated == nil {
		l.ManifestCreated = func(info ManifestCreateInfo) {}
	}
	if l.ManifestDeleted == nil {
		l.ManifestDeleted = func(info ManifestDeleteInfo) {}
	}
	if l.TableCreated == nil {
		l.TableCreated = func(info TableCreateInfo) {}
	}
	if l.TableDeleted == nil {
		l.TableDeleted = func(info TableDeleteInfo) {}
	}
	if l.TableIngested == nil {
		l.TableIngested = func(info TableIngestInfo) {}
	}
	if l.WALCreated == nil {
		l.WALCreated = func(info WALCreateInfo) {}
	}
	if l.WALDeleted == nil {
		l.WALDeleted = func(info WALDeleteInfo) {}
	}
	if l.WriteStallBegin == nil {
		l.WriteStallBegin = func(info WriteStallBeginInfo) {}
	}
	if l.WriteStallEnd == nil {
		l.WriteStallEnd = func() {}
	}
}

// MakeLoggingEventListener creates an EventListener that logs all events to the
// specified logger.
func MakeLoggingEventListener(logger Logger) EventListener {
	if logger == nil {
		logger = DefaultLogger
	}

	return EventListener{
		BackgroundError: func(err error) {
			logger.Infof("background error: %s", err)
		},
		CompactionBegin: func(info CompactionInfo) {
			logger.Infof("%s", info.String())
		},
		CompactionEnd: func(info CompactionInfo) {
			logger.Infof("%s", info.String())
		},
		FlushBegin: func(info FlushInfo) {
			logger.Infof("%s", info.String())
		},
		FlushEnd: func(info FlushInfo) {
			logger.Infof("%s", info.String())
		},
		ManifestCreated: func(info ManifestCreateInfo) {
			logger.Infof("%s", info.String())
		},
		ManifestDeleted: func(info ManifestDeleteInfo) {
			logger.Infof("%s", info.String())
		},
		TableCreated: func(info TableCreateInfo) {
			logger.Infof("%s", info.String())
		},
		TableDeleted: func(info TableDeleteInfo) {
			logger.Infof("%s", info.String())
		},
		TableIngested: func(info TableIngestInfo) {
			logger.Infof("%s", info.String())
		},
		WALCreated: func(info WALCreateInfo) {
			logger.Infof("%s", info.String())
		},
		WALDeleted: func(info WALDeleteInfo) {
			logger.Infof("%s", info.String())
		},
		WriteStallBegin: func(info WriteStallBeginInfo) {
			logger.Infof("%s", info.String())
		},
		WriteStallEnd: func() {
			logger.Infof("write stall ending")
		},
	}
}
