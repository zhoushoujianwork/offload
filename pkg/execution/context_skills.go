package execution

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

const maxSkillBytes = 16 << 20
const maxSkillEntries = 1024

type skillEntry struct {
	Path, Digest string
	Executable   bool
	Directory    bool
}

// ContextSkillDigest hashes a regular-file Skill tree with relative portable
// paths. Symlinks, special files and trees above the snapshot limits fail.
func ContextSkillDigest(root string) (string, error) {
	return snapshotSkill(root, "")
}

func validateContextSkills(skills []ContextSkill) error {
	ids, paths := make(map[string]bool), make(map[string]bool)
	for _, skill := range skills {
		if !validIDs(skill.StableID, skill.Revision) || !validDigest(skill.Digest) ||
			ids[skill.StableID] || paths[skill.MountRef] || !contextDirectory(skill.MountRef) {
			return errors.New("invalid or duplicate Skill snapshot")
		}
		ids[skill.StableID], paths[skill.MountRef] = true, true
	}
	return nil
}

func stageContextSkills(skills []ContextSkill, environment string, resume bool) ([]string, error) {
	roots := make([]string, 0, len(skills))
	for index, skill := range skills {
		// Derive filesystem names instead of using caller-provided identifiers.
		folder, _ := ContextDigest([]any{index, skill.StableID})
		destination := filepath.Join(environment, "skills", folder)
		var digest string
		var err error
		if resume {
			digest, err = snapshotSkill(destination, "")
		} else {
			digest, err = snapshotSkill(skill.MountRef, destination)
		}
		if err != nil || digest != skill.Digest {
			return nil, errors.New("Skill snapshot digest does not match")
		}
		roots = append(roots, destination)
	}
	return roots, nil
}

func snapshotSkill(root, destination string) (string, error) {
	marker, err := os.Lstat(filepath.Join(root, "SKILL.md"))
	if err != nil || !marker.Mode().IsRegular() || !contextDirectory(root) {
		return "", errors.New("Skill snapshot must have its own regular SKILL.md")
	}
	if destination != "" {
		if err := os.MkdirAll(destination, 0700); err != nil {
			return "", err
		}
	}
	var entries []skillEntry
	var size int64
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == root {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || len(entries) >= maxSkillEntries {
			return errors.New("Skill snapshot exceeds path limits")
		}
		entry := skillEntry{Path: filepath.ToSlash(rel), Directory: d.IsDir()}
		target := ""
		if destination != "" {
			target = filepath.Join(destination, rel)
		}
		if d.IsDir() {
			if target != "" {
				if err := os.Mkdir(target, 0700); err != nil {
					return err
				}
			}
		} else {
			info, err := d.Info()
			if err != nil || !info.Mode().IsRegular() || info.Size() > maxSkillBytes-size {
				return errors.New("Skill snapshot must contain bounded regular files")
			}
			data, err := readSkillFile(path, info, maxSkillBytes-size)
			if err != nil {
				return err
			}
			size += int64(len(data))
			entry.Digest, entry.Executable = ContextContentDigest(data), info.Mode().Perm()&0111 != 0
			if target != "" {
				mode := os.FileMode(0600)
				if entry.Executable {
					mode = 0700
				}
				if err := os.WriteFile(target, data, mode); err != nil {
					return err
				}
			}
		}
		entries = append(entries, entry)
		return nil
	})
	if err != nil {
		return "", err
	}
	return ContextDigest(entries)
}

func readSkillFile(path string, expected fs.FileInfo, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !os.SameFile(expected, info) {
		return nil, errors.New("Skill file changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("Skill snapshot exceeds byte limit")
	}
	return data, nil
}
