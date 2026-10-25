package library

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"

	"aos-cx-docs-dldr/internal/storage"
)

type journalState struct {
	transaction journal
	info        os.FileInfo
	data        []byte
}

func (r *Run) checkLockFile() error {
	if r.root == nil || r.lockInfo == nil {
		return errors.New("library lock ownership is not established")
	}
	if err := storage.Check(r.root, r.lock); err != nil {
		return fmt.Errorf("library lock ownership lost: %w", err)
	}
	info, err := r.root.Lstat(r.lock)
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(r.lockInfo, info) {
		return fmt.Errorf("library lock ownership lost (changed/missing inode): %s", r.lock)
	}
	return nil
}

func (r *Run) checkOwnership() error {
	if err := r.checkLockFile(); err != nil {
		return err
	}
	if !r.lockReady {
		return errors.New("library lock ownership is not fully established")
	}
	var owner lockOwner
	if err := storage.ReadJSON(r.root, r.lock, &owner); err != nil || owner != r.owner {
		return fmt.Errorf("library lock ownership lost (changed/unreadable owner token): %s", r.lock)
	}
	return r.checkLockFile()
}

// checkManagedDirectories refuses existing managed directories that another
// account could change. Mutation below them is rooted and does not follow
// symlinks, but an account that can write one of them could still swap its
// entries between this run's checks and their use.
//
// A directory must be owned by this user, by root, or by the owner of the
// destination base, who can already rename or replace any entry of the base;
// bind mounts and other filesystems that present a fixed owner therefore
// pass. Group or other write permission is refused only where the filesystem
// kept the private mode of the lock file this run just created. Filesystems
// that synthesise modes, as some Docker Desktop bind mounts do, carry no
// meaningful permission bits and are not judged by them.
func (r *Run) checkManagedDirectories() error {
	if r.lockInfo == nil {
		return errors.New("library lock ownership is not established")
	}
	base, err := r.root.Stat(".")
	if err != nil {
		return err
	}
	baseOwner, ok := storage.Owner(base)
	if !ok {
		return nil
	}
	user := os.Geteuid()
	modesKept := r.lockInfo.Mode().Perm()&0o077 == 0
	for _, name := range []string{
		r.parent, r.target,
		path.Join(r.parent, ".staging"), path.Join(r.parent, ".incomplete"),
		path.Join(r.parent, ".snapshots"), path.Join(r.parent, ".snapshots", path.Base(r.target)),
	} {
		info, err := r.root.Lstat(name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if err := managedDirectoryPolicy(filepath.Join(r.Destination, filepath.FromSlash(name)), info, user, baseOwner, modesKept); err != nil {
			return err
		}
	}
	return nil
}

func managedDirectoryPolicy(display string, info os.FileInfo, user, baseOwner int, modesKept bool) error {
	owner, ok := storage.Owner(info)
	if !ok {
		return nil
	}
	if owner != user && owner != 0 && owner != baseOwner {
		return fmt.Errorf("managed library directory %s is owned by UID %d, not by this user (UID %d), root, or the destination's owner; refusing state another account controls; choose a new base with --destination",
			display, owner, user)
	}
	if modesKept && info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("managed library directory %s is writable by other accounts (mode %#o); refusing to use it; remove group and other write permission or choose a new base with --destination",
			display, info.Mode().Perm())
	}
	return nil
}

func (r *Run) expectNoJournal() error {
	_, err := r.root.Lstat(r.journal)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("publication journal already exists; refusing to replace it: %s", r.journal)
}

func (r *Run) readJournal() (_ journalState, err error) {
	var state journalState
	if err := storage.Check(r.root, r.journal); err != nil {
		return state, err
	}
	f, err := r.root.Open(r.journal)
	if err != nil {
		return state, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	state.info, err = f.Stat()
	if err != nil {
		return state, err
	}
	if !state.info.Mode().IsRegular() {
		return state, errors.New("publication journal is not a regular file")
	}
	state.data, err = io.ReadAll(io.LimitReader(f, (8<<20)+1))
	if err != nil {
		return state, err
	}
	if len(state.data) > 8<<20 {
		return state, errors.New("publication journal exceeds size limit")
	}
	state.transaction, err = decodeJournal(state.data)
	if err != nil {
		return state, err
	}
	current, err := r.root.Lstat(r.journal)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(state.info, current) {
		return state, errors.New("publication journal changed while being read")
	}
	return state, nil
}

func (r *Run) checkTransaction(expected journalState) error {
	if err := r.checkOwnership(); err != nil {
		return err
	}
	current, err := r.readJournal()
	if err != nil {
		return fmt.Errorf("publication journal ownership lost: %w", err)
	}
	if !os.SameFile(expected.info, current.info) || !bytes.Equal(expected.data, current.data) {
		return fmt.Errorf("publication journal ownership lost (replaced or modified): %s", r.journal)
	}
	return r.checkOwnership()
}

func (r *Run) createJournal(tx journal) (journalState, error) {
	state := journalState{transaction: tx}
	data, err := jsonBytes(tx)
	if err != nil {
		return state, err
	}
	if err := r.checkOwnership(); err != nil {
		return state, err
	}
	// Exclusive creation never overwrites a foreign journal. The complete file
	// is synced and rechecked before any shared directory is moved.
	f, err := r.root.OpenFile(r.journal, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return state, fmt.Errorf("cannot exclusively create publication journal: %w", err)
	}
	state.info, err = f.Stat()
	if err != nil {
		return state, errors.Join(err, f.Close())
	}
	state.data = data
	_, writeErr := f.Write(data)
	var syncErr error
	if writeErr == nil {
		syncErr = f.Sync()
	}
	closeErr := f.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		// A partial journal is ours only while both the lock and inode remain
		// ours. Otherwise leave it for inspection rather than touching a peer.
		cleanupErr := r.checkOwnership()
		if cleanupErr == nil {
			info, statErr := r.root.Lstat(r.journal)
			if statErr != nil || !info.Mode().IsRegular() || !os.SameFile(info, state.info) {
				cleanupErr = errors.New("partial publication journal was replaced; not removed")
			} else {
				cleanupErr = errors.Join(storage.Remove(r.root, r.journal), storage.SyncDir(r.root, r.parent))
			}
		}
		return state, errors.Join(err, cleanupErr)
	}
	if err := r.checkTransaction(state); err != nil {
		return state, err
	}
	return state, storage.SyncDir(r.root, r.parent)
}

func (r *Run) renameTransaction(state journalState, from, to string) error {
	if err := r.checkTransaction(state); err != nil {
		return err
	}
	if err := storage.Check(r.root, from); err != nil {
		return err
	}
	if err := storage.Check(r.root, to); err != nil {
		return err
	}
	if _, err := r.root.Lstat(to); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("refusing publication rename onto an existing/unreadable destination %s: %v", to, err)
	}
	if err := r.checkTransaction(state); err != nil {
		return err
	}
	renameErr := r.rename(from, to)
	return errors.Join(renameErr, r.checkTransaction(state))
}

func (r *Run) rollback(state journalState) error {
	if err := r.checkTransaction(state); err != nil {
		return fmt.Errorf("rollback refused without transaction ownership: %w", err)
	}
	tx := state.transaction
	if tx.PreviousSnapshot == "" {
		return nil
	}
	previous, err := r.readLibrary(tx.PreviousSnapshot)
	if err != nil || previous.RunID != tx.PreviousRun {
		return fmt.Errorf("rollback snapshot cannot be verified: %v", err)
	}
	if err := r.renameTransaction(state, tx.PreviousSnapshot, r.target); err != nil {
		return err
	}
	return errors.Join(storage.SyncDir(r.root, r.parent), storage.SyncDir(r.root, path.Dir(tx.PreviousSnapshot)))
}

func (r *Run) removeJournal(state journalState) error {
	if err := r.checkTransaction(state); err != nil {
		return err
	}
	if err := storage.Remove(r.root, r.journal); err != nil {
		return err
	}
	return errors.Join(storage.SyncDir(r.root, r.parent), r.checkOwnership())
}
