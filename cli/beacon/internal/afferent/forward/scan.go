package forward

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/endpoint/writer"
)

// view is one retained runtime log file, opened for this scan.
type view struct {
	key  string
	path string
	f    *os.File
	size int64
	live bool
}

// entry is one consumed line. line is nil when the bytes are consumed without
// being sent: a blank line, or a line over the size limit.
type entry struct {
	key     string
	end     int64 // offset just past this line's newline
	line    []byte
	skipped bool // over MaxLineBytes
}

type batch struct {
	entries []entry
	lines   int
	bytes   int
	full    bool
}

func (b *batch) hasData() bool { return b.lines > 0 }

// openViews opens the live log and its archives, oldest first, one view per
// file identity. A path that does not exist is skipped.
func openViews(logPath string) ([]*view, error) {
	paths := writer.RetainedLogPaths(logPath) // live, .1, ..., .N
	seen := map[string]bool{}
	var views []*view
	for i, p := range paths {
		f, err := os.Open(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			closeViews(views)
			return nil, err
		}
		fi, err := f.Stat()
		if err != nil || !fi.Mode().IsRegular() {
			f.Close()
			continue
		}
		key, err := fileKey(fi)
		if err != nil {
			f.Close()
			closeViews(views)
			return nil, err
		}
		if seen[key] {
			// Seen twice while a rotation shifted it; the first sighting wins.
			f.Close()
			continue
		}
		seen[key] = true
		views = append(views, &view{key: key, path: p, f: f, size: fi.Size(), live: i == 0})
	}
	// Oldest first: .N … .1, live.
	for i, j := 0, len(views)-1; i < j; i, j = i+1, j-1 {
		views[i], views[j] = views[j], views[i]
	}
	return views, nil
}

func closeViews(vs []*view) {
	for _, v := range vs {
		v.f.Close()
	}
}

// headHash hashes the first n bytes of f.
func headHash(f *os.File, n int64) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, io.NewSectionReader(f, 0, n)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// lastLineEnd returns the offset just past the last newline in f's first
// size bytes (0 if there is none), so a first run that starts at the end of
// a file never starts in the middle of a line being written.
func lastLineEnd(f *os.File, size int64) (int64, error) {
	const chunk = 64 << 10
	buf := make([]byte, chunk)
	for end := size; end > 0; {
		start := end - chunk
		if start < 0 {
			start = 0
		}
		n, err := f.ReadAt(buf[:end-start], start)
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		if i := bytes.LastIndexByte(buf[:n], '\n'); i >= 0 {
			return start + int64(i) + 1, nil
		}
		end = start
	}
	return 0, nil
}

// readLines appends complete lines of v from off to b until b is full or the
// file (as of this scan) ends. A trailing line without a newline is left for a
// later scan. It reports whether the batch filled up.
func (fw *Forwarder) readLines(v *view, off int64, b *batch) error {
	if off >= v.size {
		return nil
	}
	r := bufio.NewReaderSize(io.NewSectionReader(v.f, off, v.size-off), 64<<10)
	pos := off
	for {
		line, n, tooBig, complete, err := readLine(r, fw.opts.MaxLineBytes)
		if err != nil {
			return err
		}
		if !complete {
			return nil
		}
		end := pos + n
		switch {
		case tooBig:
			b.entries = append(b.entries, entry{key: v.key, end: end, skipped: true})
		case len(bytes.TrimSpace(line)) == 0:
			b.entries = append(b.entries, entry{key: v.key, end: end})
		default:
			if b.lines+1 > fw.opts.MaxLines || b.bytes+len(line)+1 > fw.opts.MaxBatchBytes {
				b.full = true
				return nil
			}
			b.entries = append(b.entries, entry{key: v.key, end: end, line: line})
			b.lines++
			b.bytes += len(line) + 1
		}
		pos = end
		if b.lines >= fw.opts.MaxLines {
			b.full = true
			return nil
		}
	}
}

// readLine reads one newline-terminated line. n is the bytes consumed
// including the newline. A line longer than max is consumed but not kept
// (tooBig). complete is false when the input ends before a newline.
func readLine(r *bufio.Reader, max int) (line []byte, n int64, tooBig, complete bool, err error) {
	var buf []byte
	for {
		chunk, rerr := r.ReadSlice('\n')
		n += int64(len(chunk))
		if !tooBig {
			if len(buf)+len(chunk) > max+1 { // +1 for the newline
				tooBig, buf = true, nil
			} else {
				buf = append(buf, chunk...)
			}
		}
		switch {
		case rerr == nil:
			if !tooBig {
				buf = bytes.TrimRight(buf, "\r\n")
			}
			return buf, n, tooBig, true, nil
		case errors.Is(rerr, bufio.ErrBufferFull):
			continue
		case errors.Is(rerr, io.EOF):
			return nil, n, tooBig, false, nil
		default:
			return nil, n, tooBig, false, rerr
		}
	}
}
