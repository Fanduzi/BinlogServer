// Package tasks provides module-level functionality for tasks.
// input: the files inventory window, SelectReplayFiles, the point-in-time selection, the GTID stop selection, the executed-GTID selection, and OpenTaskSegment for each selected basename
// output: one complete ustar of those basenames, or an error and no archive when a selected segment cannot be read
// pos: replay-set archive for the authenticated download route, including the same datetime window, the same GTID stop, and the same executed-GTID roll-forward as the replay JSON
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"archive/tar"
	"errors"
	"io"
	"os"
	"time"
)

// OpenReplayArchive materializes the replay selection for limit as one ustar.
// Members are the basenames SelectReplayFiles keeps, in that order. Each
// member is opened with OpenTaskSegment, so a local file wins and a missing
// sealed UPLOADED object is the only remote source. An empty selection is an
// empty tar. If any selected segment cannot be opened or read, the error is
// returned and no archive file is left behind.
func (s *Scheduler) OpenReplayArchive(taskID string, limit int) (io.ReadCloser, int64, error) {
	members, err := s.openReplayMembers(taskID, limit)
	if err != nil {
		return nil, 0, err
	}
	return materializeReplayTar(members)
}

// OpenPITRArchive is the ustar of the same paths PITRReplay returns.
// Member bytes follow OpenTaskSegment. An empty window is an empty tar.
func (s *Scheduler) OpenPITRArchive(taskID string, start *time.Time, stop time.Time) (io.ReadCloser, int64, error) {
	selected, _, err := s.selectPITRFiles(taskID, start, stop)
	if err != nil {
		return nil, 0, err
	}
	members, err := s.openSelectedReplayMembers(taskID, selected)
	if err != nil {
		return nil, 0, err
	}
	return materializeReplayTar(members)
}

// OpenGTIDArchive is the ustar of the same paths GTIDReplay returns.
// Member bytes follow OpenTaskSegment. The stop position stays in the JSON command.
func (s *Scheduler) OpenGTIDArchive(taskID string, raw string, start *time.Time) (io.ReadCloser, int64, error) {
	selected, _, _, err := s.selectGTIDFiles(taskID, raw, start)
	if err != nil {
		return nil, 0, err
	}
	members, err := s.openSelectedReplayMembers(taskID, selected)
	if err != nil {
		return nil, 0, err
	}
	return materializeReplayTar(members)
}

// OpenExecutedGTIDArchive is the ustar of the same paths ExecutedGTIDReplay returns.
// A set that already covers every transaction up to the stop is an empty tar.
func (s *Scheduler) OpenExecutedGTIDArchive(taskID, executedRaw, stopGTIDRaw string, stop *time.Time) (io.ReadCloser, int64, error) {
	selected, _, _, _, _, _, _, err := s.selectExecutedGTID(taskID, executedRaw, stopGTIDRaw, stop)
	if err != nil {
		return nil, 0, err
	}
	members, err := s.openSelectedReplayMembers(taskID, selected)
	if err != nil {
		return nil, 0, err
	}
	return materializeReplayTar(members)
}

func materializeReplayTar(members []replayMember) (io.ReadCloser, int64, error) {
	defer closeReplayMembers(members)

	tmp, err := os.CreateTemp("", "binlog-replay-*.tar")
	if err != nil {
		return nil, 0, err
	}
	path := tmp.Name()
	remove := true
	defer func() {
		if remove {
			_ = tmp.Close()
			_ = os.Remove(path)
		}
	}()
	if err := writeReplayTar(tmp, members); err != nil {
		if errors.Is(err, tar.ErrFieldTooLong) {
			return nil, 0, ErrInvalidSegmentName
		}
		return nil, 0, err
	}
	if err := tmp.Close(); err != nil {
		return nil, 0, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, 0, err
	}
	remove = false
	return &removeOnClose{File: file, path: path}, info.Size(), nil
}

func (s *Scheduler) openReplayMembers(taskID string, limit int) ([]replayMember, error) {
	files, err := s.ListFiles(taskID, limit)
	if err != nil {
		return nil, err
	}
	if _, err := s.GetTask(taskID); err != nil {
		return nil, err
	}
	return s.openSelectedReplayMembers(taskID, SelectReplayFiles(files))
}

func (s *Scheduler) openSelectedReplayMembers(taskID string, selected []BinlogFile) ([]replayMember, error) {
	members := make([]replayMember, 0, len(selected))
	for _, file := range selected {
		name := segmentInventoryBasename(file)
		body, size, err := s.OpenTaskSegment(taskID, name)
		if err != nil {
			closeReplayMembers(members)
			return nil, err
		}
		members = append(members, replayMember{name: name, size: size, body: body})
	}
	return members, nil
}

type replayMember struct {
	name string
	size int64
	body io.ReadCloser
}

func closeReplayMembers(members []replayMember) {
	for _, member := range members {
		if member.body != nil {
			_ = member.body.Close()
		}
	}
}

func writeReplayTar(w io.Writer, members []replayMember) error {
	tw := tar.NewWriter(w)
	for _, member := range members {
		hdr := &tar.Header{
			Name:     member.name,
			Mode:     0o644,
			Size:     member.size,
			Typeflag: tar.TypeReg,
			Format:   tar.FormatUSTAR,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		n, err := io.Copy(tw, member.body)
		if err != nil {
			return err
		}
		if n != member.size {
			return io.ErrUnexpectedEOF
		}
	}
	return tw.Close()
}

type removeOnClose struct {
	*os.File
	path string
}

func (f *removeOnClose) Close() error {
	err := f.File.Close()
	if rmErr := os.Remove(f.path); err == nil {
		err = rmErr
	}
	return err
}
