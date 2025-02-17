// Copyright 2024 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package wal

import (
	"bytes"
	"cmp"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/pebble/batchrepr"
	"github.com/cockroachdb/pebble/internal/base"
	"github.com/cockroachdb/pebble/record"
	"github.com/cockroachdb/pebble/vfs"
)

// A segment represents an individual physical file that makes up a contiguous
// segment of a logical WAL. If a failover occurred during a WAL's lifetime, a
// WAL may be composed of multiple segments.
type segment struct {
	logNameIndex logNameIndex
	dir          Dir
}

func (s segment) String() string {
	return fmt.Sprintf("(%s,%s)", s.dir.Dirname, s.logNameIndex)
}

// A logicalWAL identifies a logical WAL and its consituent segment files.
type logicalWAL struct {
	NumWAL
	// segments contains the list of the consistuent physical segment files that
	// make up the single logical WAL file. segments is ordered by increasing
	// logIndex.
	segments []segment
}

func (w logicalWAL) String() string {
	var sb strings.Builder
	sb.WriteString(base.DiskFileNum(w.NumWAL).String())
	sb.WriteString(": {")
	for i := range w.segments {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(w.segments[i].String())
	}
	sb.WriteString("}")
	return sb.String()
}

type logicalWALs []logicalWAL

// get retrieves the WAL with the given number if present. The second return
// value indicates whether or not the WAL was found.
func (wals logicalWALs) get(num NumWAL) (logicalWAL, bool) {
	i, found := slices.BinarySearchFunc(wals, num, func(lw logicalWAL, n NumWAL) int {
		return cmp.Compare(lw.NumWAL, n)
	})
	if !found {
		return logicalWAL{}, false
	}
	return wals[i], true
}

// listLogs finds all log files in the provided directories. It returns an
// ordered list of WALs in increasing NumWAL order.
func listLogs(dirs ...Dir) (logicalWALs, error) {
	var wals []logicalWAL
	for _, d := range dirs {
		ls, err := d.FS.List(d.Dirname)
		if err != nil {
			return nil, errors.Wrapf(err, "reading %q", d.Dirname)
		}
		for _, name := range ls {
			dfn, li, ok := parseLogFilename(name)
			if !ok {
				continue
			}
			// Have we seen this logical log number yet?
			i, found := slices.BinarySearchFunc(wals, dfn, func(lw logicalWAL, n NumWAL) int {
				return cmp.Compare(lw.NumWAL, n)
			})
			if !found {
				wals = slices.Insert(wals, i, logicalWAL{NumWAL: dfn, segments: make([]segment, 0, 1)})
			}

			// Ensure we haven't seen this log index yet, and find where it
			// slots within this log's segments.
			j, found := slices.BinarySearchFunc(wals[i].segments, li, func(s segment, li logNameIndex) int {
				return cmp.Compare(s.logNameIndex, li)
			})
			if found {
				return nil, errors.Errorf("wal: duplicate logIndex=%s for WAL %s in %s and %s",
					li, dfn, d.Dirname, wals[i].segments[j].dir.Dirname)
			}
			wals[i].segments = slices.Insert(wals[i].segments, j, segment{logNameIndex: li, dir: d})
		}
	}
	return wals, nil
}

func newVirtualWALReader(logNum NumWAL, segments []segment) *virtualWALReader {
	return &virtualWALReader{
		logNum:    logNum,
		segments:  segments,
		currIndex: -1,
	}
}

// A virtualWALReader takes an ordered sequence of physical WAL files
// ("segments") and implements the wal.Reader interface, providing a merged view
// of the WAL's logical contents. It's responsible for filtering duplicate
// records which may be shared by the tail of a segment file and the head of its
// successor.
type virtualWALReader struct {
	// VirtualWAL metadata.
	logNum   NumWAL
	segments []segment

	// State pertaining to the current position of the reader within the virtual
	// WAL and its constituent physical files.
	currIndex  int
	currFile   vfs.File
	currReader *record.Reader
	// off describes the current Offset within the WAL.
	off Offset
	// lastSeqNum is the sequence number of the batch contained within the last
	// record returned to the user. A virtual WAL may be split across a sequence
	// of several physical WAL files. The tail of one physical WAL may be
	// duplicated within the head of the next physical WAL file. We use
	// contained batches' sequence numbers to deduplicate. This lastSeqNum field
	// should monotonically increase as we iterate over the WAL files. If we
	// ever observe a batch encoding a sequence number <= lastSeqNum, we must
	// have already returned the batch and should skip it.
	lastSeqNum uint64
	// recordBuf is a buffer used to hold the latest record read from a physical
	// file, and then returned to the user. A pointer to this buffer is returned
	// directly to the caller of NextRecord.
	recordBuf bytes.Buffer
}

// *virtualWALReader implements wal.Reader.
var _ Reader = (*virtualWALReader)(nil)

// NextRecord returns a reader for the next record. It returns io.EOF if there
// are no more records. The reader returned becomes stale after the next
// NextRecord call, and should no longer be used.
func (r *virtualWALReader) NextRecord() (io.Reader, Offset, error) {
	r.recordBuf.Reset()

	// On the first call, we need to open the first file.
	if r.currIndex < 0 {
		err := r.nextFile()
		if err != nil {
			return nil, Offset{}, err
		}
	}

	for {
		// Update our current physical offset to match the current file offset.
		r.off.Physical = r.currReader.Offset()
		// Obtain a Reader for the next record within this log file.
		rec, err := r.currReader.Next()
		if errors.Is(err, io.EOF) {
			// This file is exhausted; continue to the next.
			err := r.nextFile()
			if err != nil {
				return nil, r.off, err
			}
			continue
		}

		// Copy the record into a buffer. This ensures we read its entirety so
		// that NextRecord returns the next record, even if the caller never
		// exhausts the previous record's Reader. The record.Reader requires the
		// record to be exhausted to read all of the record's chunks before
		// attempting to read the next record. Buffering also also allows us to
		// easily read the header of the batch down below for deduplication.
		if err == nil {
			_, err = io.Copy(&r.recordBuf, rec)
		}
		// The record may be malformed. This is expected during a WAL failover,
		// because the tail of a WAL may be only partially written or otherwise
		// unclean because of WAL recycling and the inability to write the EOF
		// trailer record. If this isn't the last file, we silently ignore the
		// invalid record at the tail and proceed to the next file. If it is
		// the last file, bubble the error up and let the client decide what to
		// do with it. If the virtual WAL is the most recent WAL, Open may also
		// decide to ignore it because it's consistent with an incomplete
		// in-flight write at the time of process exit/crash. See #453.
		if record.IsInvalidRecord(err) && r.currIndex < len(r.segments)-1 {
			if err := r.nextFile(); err != nil {
				return nil, r.off, err
			}
			continue
		} else if err != nil {
			return nil, r.off, err
		}

		// We may observe repeat records between the physical files that make up
		// a virtual WAL because inflight writes to a file on a stalled disk may
		// or may not end up completing. WAL records always contain encoded
		// batches, and batches that contain data can be uniquely identifed by
		// sequence number.
		//
		// Parse the batch header.
		h, ok := batchrepr.ReadHeader(r.recordBuf.Bytes())
		if !ok {
			// Failed to read the batch header because the record was smaller
			// than the length of a batch header. This is unexpected. The record
			// envelope successfully decoded and the checkums of the individual
			// record fragment(s) validated, so the writer truly wrote an
			// invalid batch. During Open WAL recovery treats this as
			// corruption. We could return the record to the caller, allowing
			// the caller to interpret it as corruption, but it seems safer to
			// be explicit and surface the corruption error here.
			return nil, r.off, base.CorruptionErrorf("pebble: corrupt log file logNum=%d, logNameIndex=%s: invalid batch",
				r.logNum, errors.Safe(r.segments[r.currIndex].logNameIndex))
		}

		// There's a subtlety necessitated by LogData operations. A LogData
		// applied to a batch results in data appended to the WAL in a batch
		// format, but the data is never applied to the memtable or LSM. A batch
		// only containing LogData will repeat a sequence number. We skip these
		// batches because they're not relevant for recovery and we do not want
		// to mistakenly deduplicate the batch containing KVs at the same
		// sequence number. We can differentiate LogData-only batches through
		// their batch headers: they'll encode a count of zero.
		if h.Count == 0 {
			r.recordBuf.Reset()
			continue
		}

		// If we've already observed a sequence number >= this batch's sequence
		// number, we must've already returned this record to the client. Skip
		// it.
		if h.SeqNum <= r.lastSeqNum {
			r.recordBuf.Reset()
			continue
		}
		r.lastSeqNum = h.SeqNum
		return &r.recordBuf, r.off, nil
	}
}

// Close closes the reader, releasing open resources.
func (r *virtualWALReader) Close() error {
	if r.currFile != nil {
		if err := r.currFile.Close(); err != nil {
			return err
		}
	}
	return nil
}

// nextFile advances the internal state to the next physical segment file.
func (r *virtualWALReader) nextFile() error {
	if r.currFile != nil {
		err := r.currFile.Close()
		r.currFile = nil
		if err != nil {
			return err
		}
	}
	r.currIndex++
	if r.currIndex >= len(r.segments) {
		return io.EOF
	}

	segment := r.segments[r.currIndex]
	fs := segment.dir.FS
	path := fs.PathJoin(segment.dir.Dirname, makeLogFilename(r.logNum, segment.logNameIndex))
	r.off.PhysicalFile = path
	r.off.Physical = 0
	var err error
	if r.currFile, err = fs.Open(path); err != nil {
		return errors.Wrapf(err, "opening WAL file segment %q", path)
	}
	r.currReader = record.NewReader(r.currFile, base.DiskFileNum(r.logNum))
	return nil
}
