package supervisor

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/makez-dev/makez/internal/core"
)

func (s *Supervisor) outputGap(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.load(id)
	if err == nil && state != nil {
		state.OutputGap = true
		_ = s.save(id, state)
	}
}

func (s *Supervisor) appendOutput(file *os.File, stream string, data []byte) error {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	offset, err := file.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	record := core.OutputRecord{Offset: strconv.FormatInt(offset, 10), Stream: stream, Data: data}
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if offset+int64(len(encoded))+1 > maxOutput {
		return errors.New("output limit reached")
	}
	if _, err = file.Write(append(encoded, '\n')); err != nil {
		return err
	}
	return file.Sync()
}

func (s *Supervisor) readOutput(id, stream string, pipe *os.File, literals [][]byte) {
	defer pipe.Close()
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		state, err := s.load(id)
		if err == nil && state != nil {
			state.Readers = max(0, state.Readers-1)
			_ = s.save(id, state)
		}
	}()
	file, err := os.OpenFile(filepath.Join(s.control, id+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		s.outputGap(id)
		_, _ = io.Copy(io.Discard, pipe)
		return
	}
	defer file.Close()
	filter := redactor{values: literals}
	buffer := make([]byte, 8192)
	failed := false
	for {
		n, readErr := pipe.Read(buffer)
		safe := filter.feed(buffer[:n], readErr != nil)
		for len(safe) > 0 && !failed {
			length := min(len(safe), 8192)
			if err := s.appendOutput(file, stream, safe[:length]); err != nil {
				s.outputGap(id)
				failed = true
			}
			safe = safe[length:]
		}
		// Keep draining after a spool failure so cancellation can still stop a noisy child.
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				s.outputGap(id)
			}
			return
		}
	}
}

func (s *Supervisor) Output(key core.OperationKey, cursor string, maximum int) (core.OutputPage, error) {
	var page core.OutputPage
	id, err := identity(key)
	if err != nil {
		return page, err
	}
	var offset int64
	if cursor != "" {
		offset, err = strconv.ParseInt(cursor, 10, 64)
	}
	if err != nil || offset < 0 {
		return page, invalid("invalid output cursor")
	}
	s.mu.Lock()
	state, err := s.load(id)
	s.mu.Unlock()
	if err != nil {
		return page, err
	}
	if state == nil {
		return page, &fault{core.OutcomeUnknown, "execution receipt missing"}
	}
	if state.OutputGap {
		return page, &fault{core.OutputGap, "durable output is incomplete"}
	}
	if maximum == 0 {
		maximum = 131072
	}
	maximum = min(1<<20, max(16384, maximum))
	s.logMu.Lock()
	defer s.logMu.Unlock()
	page.Records = []core.OutputRecord{}
	file, err := os.Open(filepath.Join(s.control, id+".log"))
	var size int64
	if errors.Is(err, os.ErrNotExist) {
		if offset != 0 {
			return page, &fault{core.OutputGap, "output file is missing"}
		}
	} else if err != nil {
		return page, err
	} else {
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			return page, err
		}
		size = info.Size()
		if offset > size {
			return page, &fault{core.OutputGap, "cursor exceeds durable output"}
		}
		if _, err = file.Seek(offset, io.SeekStart); err != nil {
			return page, err
		}
		reader := bufio.NewReaderSize(file, 16384)
		for total := 0; total < maximum; {
			line, err := reader.ReadSlice('\n')
			if len(line) == 0 && errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return page, &fault{core.OutputGap, "output cursor is not a record boundary"}
			}
			if len(page.Records) > 0 && total+len(line) > maximum {
				break
			}
			var record core.OutputRecord
			if json.Unmarshal(line, &record) != nil || record.Offset != strconv.FormatInt(offset, 10) {
				return page, &fault{core.OutputGap, "output cursor is not a record boundary"}
			}
			page.Records = append(page.Records, record)
			total += len(line)
			offset += int64(len(line))
		}
	}
	page.NextCursor = strconv.FormatInt(offset, 10)
	page.EOF = (state.State == "EXITED" || state.State == "STOPPED") && state.Readers == 0 && offset == size
	return page, nil
}
