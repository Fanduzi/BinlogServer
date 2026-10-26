// Package tasks provides module-level functionality for tasks.
// input: the files inventory window, SelectReplayFiles, the point-in-time selection, the GTID stop selection, the executed-GTID selection, and OpenTaskSegment for each selected basename; replayArchiveTempDir chooses where the temp tar is created (empty: the process temp dir)
// output: one complete ustar of those basenames, or an error and no archive when a selected segment cannot be read; a damaged segment and every later one are not archived; an open member sealed after the selection is read under its sealed name or selected again (#322); replayArchiveTempDir stores the directory archive temps are created in so a concurrent reader does not race the write
// pos: replay-set archive for the authenticated download route, including the same datetime window, the same GTID stop, and the same executed-GTID roll-forward as the replay JSON
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"archive/tar"
	"errors"
	"io"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"binlog_server/internal/binlog"
)

// OpenReplayArchive materializes the replay selection for limit as one ustar.
// Members are the basenames SelectReplayFiles keeps, in that order. Each
// member is opened with OpenTaskSegment, so a local file wins and a missing
// sealed UPLOADED object is the only remote source. An empty selection is an
// empty tar. If any selected segment cannot be opened or read, the error is
// returned and no archive file is left behind.
func (s *Scheduler) OpenReplayArchive(taskID string, limit int) (io.ReadCloser, int64, error) {
	return s.openReplayArchive(taskID, func() ([]BinlogFile, error) {
		return s.selectReplayFiles(taskID, limit)
	})
}

// OpenPITRArchive is the ustar of the same paths PITRReplay returns.
// Member bytes follow OpenTaskSegment. An empty window is an empty tar.
func (s *Scheduler) OpenPITRArchive(taskID string, start *time.Time, stop time.Time) (io.ReadCloser, int64, error) {
	return s.openReplayArchive(taskID, func() ([]BinlogFile, error) {
		selected, _, err := s.selectPITRFiles(taskID, start, stop)
		return selected, err
	})
}

// OpenGTIDArchive is the ustar of the same paths GTIDReplay returns.
// Member bytes follow OpenTaskSegment. The stop position stays in the JSON command.
func (s *Scheduler) OpenGTIDArchive(taskID string, raw string, start *time.Time) (io.ReadCloser, int64, error) {
	return s.openReplayArchive(taskID, func() ([]BinlogFile, error) {
		selected, _, _, err := s.selectGTIDFiles(taskID, raw, start)
		return selected, err
	})
}

// OpenExecutedGTIDArchive is the ustar of the same paths ExecutedGTIDReplay returns.
// A set that already covers every transaction up to the stop is an empty tar.
func (s *Scheduler) OpenExecutedGTIDArchive(taskID, executedRaw, stopGTIDRaw string, stop *time.Time) (io.ReadCloser, int64, error) {
	return s.openReplayArchive(taskID, func() ([]BinlogFile, error) {
		selected, _, _, _, _, _, _, err := s.selectExecutedGTID(taskID, executedRaw, stopGTIDRaw, stop)
		return selected, err
	})
}

// replayArchiveTempDir, when set, is the directory archive temps are created in.
// Empty uses the process temp directory (production). The tasks test binary
// sets it so a leftover count is not shared with another package's tests,
// which run in parallel and use the same process temp directory.
var replayArchiveTempDir atomic.Value // string

func replayArchiveCreateDir() string {
	v, _ := replayArchiveTempDir.Load().(string)
	return v
}

func materializeReplayTar(members []replayMember) (io.ReadCloser, int64, error) {
	defer closeReplayMembers(members)

	tmp, err := os.CreateTemp(replayArchiveCreateDir(), "binlog-replay-*.tar")
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

// openReplayArchive selects the replay files, opens every member, and writes
// the tar. A RUNNING task can seal its open segment (rename name.open.eN to
// the sealed name) after the selection listed it and before it is opened.
// That member is then gone under the listed name, so the whole selection is
// made again from a fresh listing, which names the sealed file (#322). Once
// every member is open, a later rename does not matter: the open handles keep
// the bytes, capped at the size seen at open.
func (s *Scheduler) openReplayArchive(taskID string, selectFiles func() ([]BinlogFile, error)) (io.ReadCloser, int64, error) {
	var lastErr error
	for attempt := 0; attempt < replayArchiveAttempts; attempt++ {
		selected, err := selectFiles()
		var sealed *replayMemberSealedError
		if errors.As(err, &sealed) {
			// The point-in-time and GTID selections read the segments too.
			lastErr = sealed.err
			time.Sleep(time.Duration(attempt+1) * replayArchiveRetryStep)
			continue
		}
		if err != nil {
			return nil, 0, err
		}
		if hook := replayArchiveAfterSelect; hook != nil {
			hook(taskID, selected)
		}
		members, err := s.openSelectedReplayMembers(taskID, selected)
		if err == nil {
			return materializeReplayTar(members)
		}
		if !errors.As(err, &sealed) {
			return nil, 0, err
		}
		// After the last attempt the member is really missing on this
		// process (for example an OPEN catalog row whose file is elsewhere),
		// so the caller gets the open error itself, not a retry hint.
		lastErr = sealed.err
		// With a catalog, the seal updates the row just after the rename.
		time.Sleep(time.Duration(attempt+1) * replayArchiveRetryStep)
	}
	return nil, 0, lastErr
}

// replayArchiveRetryStep is the pause before the next selection, times the
// attempt number.
const replayArchiveRetryStep = 20 * time.Millisecond

// replayArchiveAttempts bounds how often one archive request selects again
// because an open member was sealed in between.
const replayArchiveAttempts = 5

// replayMemberSealedError means a selected .open.eN member was not found
// under its listed name, normally because a seal renamed it after the
// selection. openReplayArchive selects again. err is the open error; the
// message is err's own, so a caller that does not retry (the replay JSON
// routes) answers exactly as before.
type replayMemberSealedError struct {
	name string
	err  error
}

func (e *replayMemberSealedError) Error() string { return e.err.Error() }

func (e *replayMemberSealedError) Unwrap() error { return e.err }

// replayArchiveAfterSelect runs between selection and member open. Tests set
// it to seal a selected open segment at that point; it is nil otherwise.
var replayArchiveAfterSelect func(taskID string, selected []BinlogFile)

func (s *Scheduler) selectReplayFiles(taskID string, limit int) ([]BinlogFile, error) {
	files, err := s.ListFiles(taskID, limit)
	if err != nil {
		return nil, err
	}
	task, err := s.GetTask(taskID)
	if err != nil {
		return nil, err
	}
	chosen, _ := s.SelectReplayFilesFor(task, files)
	return chosen, nil
}

// openSelectedReplayMembers opens each selected basename. selected already
// comes from ListFiles, so it is the inventory: the files are not listed
// again per member (OpenTaskSegment would re-read the directory each time).
func (s *Scheduler) openSelectedReplayMembers(taskID string, selected []BinlogFile) ([]replayMember, error) {
	members := make([]replayMember, 0, len(selected))
	for _, file := range selected {
		name, body, size, err := s.openSelectedSegment(taskID, selected, file)
		if err != nil {
			closeReplayMembers(members)
			return nil, err
		}
		members = append(members, replayMember{name: name, size: size, body: body})
	}
	return members, nil
}

// openSelectedSegment opens one selected file against the inventory it was
// selected from, without listing the directory again. When an .open.eN
// member is gone because a seal renamed it after the listing, the sealed
// file it became is opened instead and its name is returned. When that file
// is not on this process either, the error is a *replayMemberSealedError
// that still matches ErrSegmentNotOnProcess, so an archive selects again and
// every other caller keeps the 404.
func (s *Scheduler) openSelectedSegment(taskID string, inventory []BinlogFile, file BinlogFile) (string, io.ReadCloser, int64, error) {
	name := segmentInventoryBasename(file)
	body, size, err := s.openInventorySegment(taskID, inventory, name)
	if err == nil || !binlog.OpenName(name) || !errors.Is(err, ErrSegmentNotOnProcess) {
		return name, body, size, err
	}
	if sealedName, sealedBody, sealedSize, ok := s.openSealedSuccessor(taskID, file, name, inventory); ok {
		return sealedName, sealedBody, sealedSize, nil
	}
	return name, nil, 0, &replayMemberSealedError{name: name, err: err}
}

// openSealedSuccessor opens the sealed file an open segment became. The
// runner seals name.open.eN as name, or as name.sealed.eN when name already
// exists, so name.sealed.eN is tried first. A sealed name that is already
// in the selection is a different (earlier) segment and is not used. The
// sealed file holds every byte the open file had, plus the events written
// until the seal, so a plain name that is shorter than the listed open file
// or last written before it was listed is an earlier epoch left outside the
// selection, not this seal, and is not used either. ok is false when no
// successor is on this process; the caller then selects again.
func (s *Scheduler) openSealedSuccessor(taskID string, open BinlogFile, openName string, selected []BinlogFile) (string, io.ReadCloser, int64, bool) {
	named, ok := binlog.ClassifySegment(openName)
	if !ok || !named.Open {
		return "", nil, 0, false
	}
	epochName := named.Source + ".sealed.e" + strconv.FormatInt(named.Epoch, 10)
	for _, candidate := range []string{epochName, named.Source} {
		if inventoryIncludesBasename(selected, candidate) {
			continue
		}
		body, size, err := s.openLocalTaskSegment(taskID, candidate)
		if err != nil {
			continue
		}
		if candidate == named.Source && !sealedSuccessorOf(body, size, open) {
			_ = body.Close()
			continue
		}
		return candidate, body, size, true
	}
	return "", nil, 0, false
}

// sealedSuccessorOf reports whether the plain sealed file behind body can be
// the listed open segment after its seal: at least as long, and modified no
// earlier than the listing saw it (CreatedAt is the open file's mtime on
// disk, or the catalog row time, which is truncated to the second).
func sealedSuccessorOf(body io.ReadCloser, size int64, open BinlogFile) bool {
	if size < open.SizeBytes {
		return false
	}
	if open.CreatedAt.IsZero() {
		return true
	}
	lf, ok := body.(*limitedFile)
	if !ok {
		return false
	}
	info, err := lf.File.Stat()
	if err != nil {
		return false
	}
	return !info.ModTime().Before(open.CreatedAt.Truncate(time.Second))
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
