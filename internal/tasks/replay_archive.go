// Package tasks provides module-level functionality for tasks.
// input: the files inventory window, SelectReplayFiles, and OpenTaskSegment for each selected basename
// output: one complete ustar of those basenames, or an error and no archive when a selected segment cannot be read
// pos: replay-set archive for the authenticated download route; single-segment download and replay JSON stay unchanged
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"archive/tar"
	"errors"
	"io"
	"os"
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
	selected := SelectReplayFiles(files)
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
