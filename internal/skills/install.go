package skills

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nonlinear-xyz/shale/internal/store"
)

func AddTarget(ctx context.Context, db *store.DB, name, directory string) (store.SkillTarget, error) {
	if !filepath.IsAbs(directory) {
		return store.SkillTarget{}, errors.New("skill target directory must be absolute")
	}
	path := filepath.Clean(directory)
	if IsPluginCachePath(path) {
		return store.SkillTarget{}, errors.New("plugin cache directories are read-only deployment artifacts and cannot be install targets")
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return store.SkillTarget{}, errors.New("skill target must be a real directory, not a symlink")
		}
	} else if os.IsNotExist(err) {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return store.SkillTarget{}, err
		}
	} else {
		return store.SkillTarget{}, err
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return store.SkillTarget{}, err
	}
	if IsPluginCachePath(canonical) {
		return store.SkillTarget{}, errors.New("resolved target is inside a plugin cache")
	}
	volumeRoot := filepath.VolumeName(canonical) + string(filepath.Separator)
	home, _ := os.UserHomeDir()
	if canonical == volumeRoot || (home != "" && canonical == filepath.Clean(home)) {
		return store.SkillTarget{}, errors.New("skill target cannot be a filesystem root or the home directory")
	}
	return db.AddSkillTarget(ctx, strings.TrimSpace(name), canonical, "human")
}

type InstallResult struct {
	Installation store.SkillInstallation
	PreviousTree string
	Warnings     []string
}

func Install(ctx context.Context, db *store.DB, ref store.SkillRef, targetName string) (InstallResult, error) {
	if ref.TreeHash == "" {
		return InstallResult{}, errors.New("installation requires an exact revision ref with @<tree-hash>")
	}
	detail, err := db.ResolveSkillRef(ctx, ref)
	if err != nil {
		return InstallResult{}, err
	}
	if detail.Status == store.SkillDraft {
		return InstallResult{}, errors.New("draft skills cannot be installed; activate the draft first")
	}
	target, err := db.SkillTargetByName(ctx, strings.TrimSpace(targetName))
	if err != nil {
		return InstallResult{}, err
	}
	if IsPluginCachePath(target.Path) {
		return InstallResult{}, errors.New("plugin cache directories cannot be install targets")
	}
	info, err := os.Lstat(target.Path)
	if err != nil {
		return InstallResult{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return InstallResult{}, errors.New("configured target is no longer a real directory")
	}
	destination := filepath.Join(target.Path, detail.Name)
	if err := ensureWithin(target.Path, destination); err != nil {
		return InstallResult{}, err
	}
	prior, priorErr := db.SkillInstallation(ctx, target.ID, detail.LibraryID, detail.Name)
	destinationExists := false
	if info, err := os.Lstat(destination); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return InstallResult{}, errors.New("install destination is not a real directory")
		}
		destinationExists = true
	} else if !os.IsNotExist(err) {
		return InstallResult{}, err
	}
	if destinationExists {
		if priorErr != nil {
			if errors.Is(priorErr, os.ErrNotExist) {
				return InstallResult{}, fmt.Errorf("refusing to overwrite unmanaged directory %s", destination)
			}
			return InstallResult{}, priorErr
		}
		if prior.InstalledPath != destination {
			return InstallResult{}, errors.New("installation record does not match the target path")
		}
		actual, err := SnapshotDir(destination, detail.Name)
		if err != nil {
			return InstallResult{}, fmt.Errorf("verify installed baseline: %w", err)
		}
		actualHash, err := store.ComputeSkillTreeHash(actual.Files)
		if err != nil {
			return InstallResult{}, err
		}
		if actualHash != prior.TreeHash {
			return InstallResult{}, errors.New("installed skill was modified outside Shale; refusing to overwrite the divergent baseline")
		}
	} else if priorErr == nil {
		return InstallResult{}, errors.New("managed installation directory is missing; remove or repair its target record before reinstalling")
	} else if !errors.Is(priorErr, os.ErrNotExist) {
		return InstallResult{}, priorErr
	}

	files, err := db.SkillRevisionInputs(ctx, detail)
	if err != nil {
		return InstallResult{}, err
	}
	stage, err := os.MkdirTemp(target.Path, ".shale-stage-")
	if err != nil {
		return InstallResult{}, err
	}
	stagePresent := true
	defer func() {
		if stagePresent {
			_ = os.RemoveAll(stage)
		}
	}()
	for _, file := range files {
		path := filepath.Join(stage, filepath.FromSlash(file.Path))
		if err := ensureWithin(stage, path); err != nil {
			return InstallResult{}, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return InstallResult{}, err
		}
		if err := os.WriteFile(path, file.Content, file.Mode.Perm()); err != nil {
			return InstallResult{}, err
		}
	}
	staged, err := SnapshotDir(stage, detail.Name)
	if err != nil {
		return InstallResult{}, fmt.Errorf("validate staged install: %w", err)
	}
	stagedHash, err := store.ComputeSkillTreeHash(staged.Files)
	if err != nil {
		return InstallResult{}, err
	}
	if stagedHash != detail.TreeHash {
		return InstallResult{}, errors.New("staged install did not reproduce the exact requested revision")
	}

	backup := ""
	if destinationExists {
		backup = filepath.Join(target.Path, ".shale-backup-"+detail.Name+"-"+detail.TreeHash[:8])
		if _, err := os.Lstat(backup); err == nil {
			return InstallResult{}, fmt.Errorf("stale install backup already exists at %s", backup)
		} else if !os.IsNotExist(err) {
			return InstallResult{}, err
		}
		if err := os.Rename(destination, backup); err != nil {
			return InstallResult{}, err
		}
	}
	if err := os.Rename(stage, destination); err != nil {
		if backup != "" {
			_ = os.Rename(backup, destination)
		}
		return InstallResult{}, err
	}
	stagePresent = false
	installation, err := db.RecordSkillInstallation(ctx, target, detail, destination, "human")
	if err != nil {
		_ = os.RemoveAll(destination)
		if backup != "" {
			_ = os.Rename(backup, destination)
		}
		return InstallResult{}, fmt.Errorf("record installation (filesystem rolled back): %w", err)
	}
	result := InstallResult{Installation: installation}
	if priorErr == nil {
		result.PreviousTree = prior.TreeHash
	}
	if backup != "" {
		if err := os.RemoveAll(backup); err != nil {
			result.Warnings = append(result.Warnings, "installed successfully but could not remove backup "+backup)
		}
	}
	return result, nil
}
