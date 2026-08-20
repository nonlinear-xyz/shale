// Package skills implements Shale's local-first skill library lifecycle.
// SQLite is the catalog and review queue; exact package files remain the
// canonical content. No validation command found inside a skill is executed.
package skills

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nonlinear-xyz/shale/internal/store"
)

type Metadata struct {
	Name        string
	Description string
}

type Snapshot struct {
	Name        string
	Description string
	Status      store.SkillStatus
	SourcePath  string // ephemeral local discovery path; never persisted or emitted
	Files       []store.SkillFileInput
	Warnings    []string
}

var markdownLink = regexp.MustCompile(`\]\(([^)]+)\)`)

func ValidateSkillMD(body []byte, expectedName string) (Metadata, []string, error) {
	if !utf8.Valid(body) {
		return Metadata{}, nil, errors.New("SKILL.md must be UTF-8")
	}
	if len(body) == 0 || len(body) > 1<<20 {
		return Metadata{}, nil, errors.New("SKILL.md must be between 1 byte and 1 MiB")
	}
	text := strings.ReplaceAll(string(body), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) < 3 || strings.TrimSpace(lines[0]) != "---" {
		return Metadata{}, nil, errors.New("SKILL.md requires YAML frontmatter delimited by ---")
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return Metadata{}, nil, errors.New("SKILL.md frontmatter is not closed with ---")
	}
	values := map[string]string{}
	for i := 1; i < end; i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if len(line) != len(strings.TrimLeft(line, " \t")) {
			// Nested metadata is allowed but cannot define routing fields.
			continue
		}
		colon := strings.IndexByte(line, ':')
		if colon <= 0 {
			return Metadata{}, nil, fmt.Errorf("invalid frontmatter line %d", i+1)
		}
		key := strings.TrimSpace(line[:colon])
		value := strings.TrimSpace(line[colon+1:])
		if key != "name" && key != "description" {
			continue
		}
		if _, exists := values[key]; exists {
			return Metadata{}, nil, fmt.Errorf("frontmatter field %q is repeated", key)
		}
		if value == "|" || value == ">" || value == "|-" || value == ">-" || value == "|+" || value == ">+" {
			var block []string
			for i+1 < end {
				next := lines[i+1]
				if strings.TrimSpace(next) != "" && len(next) == len(strings.TrimLeft(next, " \t")) {
					break
				}
				i++
				block = append(block, strings.TrimSpace(next))
			}
			sep := "\n"
			if strings.HasPrefix(value, ">") {
				sep = " "
			}
			value = strings.TrimSpace(strings.Join(block, sep))
		} else {
			decoded, err := decodeYAMLScalar(value)
			if err != nil {
				return Metadata{}, nil, fmt.Errorf("frontmatter %s: %w", key, err)
			}
			value = decoded
		}
		values[key] = value
	}
	meta := Metadata{Name: strings.TrimSpace(values["name"]), Description: strings.TrimSpace(values["description"])}
	if !store.ValidSkillName(meta.Name) {
		return Metadata{}, nil, fmt.Errorf("frontmatter name %q must be lowercase hyphen-case and under 64 characters", meta.Name)
	}
	if expectedName != "" && meta.Name != expectedName {
		return Metadata{}, nil, fmt.Errorf("frontmatter name %q does not match skill folder %q", meta.Name, expectedName)
	}
	if meta.Description == "" {
		return Metadata{}, nil, errors.New("frontmatter description is required")
	}
	if len(meta.Description) > 16<<10 {
		return Metadata{}, nil, errors.New("frontmatter description exceeds 16 KiB")
	}
	if strings.TrimSpace(strings.Join(lines[end+1:], "\n")) == "" {
		return Metadata{}, nil, errors.New("SKILL.md requires an instruction body after frontmatter")
	}
	var warnings []string
	if len(lines) > 500 {
		warnings = append(warnings, fmt.Sprintf("SKILL.md has %d lines; consider moving detail into references", len(lines)))
	}
	return meta, warnings, nil
}

func decodeYAMLScalar(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if strings.HasPrefix(value, `"`) {
		decoded, err := strconv.Unquote(value)
		if err != nil {
			return "", errors.New("invalid double-quoted value")
		}
		return decoded, nil
	}
	if strings.HasPrefix(value, "'") {
		if len(value) < 2 || !strings.HasSuffix(value, "'") {
			return "", errors.New("invalid single-quoted value")
		}
		return strings.ReplaceAll(value[1:len(value)-1], "''", "'"), nil
	}
	if hash := strings.Index(value, " #"); hash >= 0 {
		value = value[:hash]
	}
	return strings.TrimSpace(value), nil
}

func SnapshotDir(path, expectedName string) (Snapshot, error) {
	root, err := filepath.Abs(path)
	if err != nil {
		return Snapshot{}, err
	}
	info, err := os.Lstat(root)
	if err != nil {
		return Snapshot{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return Snapshot{}, errors.New("skill root must be a real directory, not a symlink")
	}
	var files []store.SkillFileInput
	total := int64(0)
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("skill tree contains symlink %q", rel)
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("skill tree contains special file %q", rel)
		}
		if info.Size() > 16<<20 {
			return fmt.Errorf("skill file %q exceeds 16 MiB", rel)
		}
		total += info.Size()
		if total > 64<<20 {
			return errors.New("skill tree exceeds 64 MiB")
		}
		if len(files) >= 4096 {
			return errors.New("skill tree exceeds 4096 files")
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files = append(files, store.SkillFileInput{Path: rel, Content: body, Mode: info.Mode()})
		return nil
	})
	if err != nil {
		return Snapshot{}, err
	}
	snapshot, err := ValidateFiles(files, expectedName)
	if err != nil {
		return Snapshot{}, err
	}
	snapshot.SourcePath = root
	return snapshot, nil
}

func ValidateFiles(files []store.SkillFileInput, expectedName string) (Snapshot, error) {
	if len(files) == 0 {
		return Snapshot{}, errors.New("skill tree is empty")
	}
	paths := map[string]bool{}
	var skillMD []byte
	for _, file := range files {
		path := filepath.ToSlash(filepath.Clean(file.Path))
		if filepath.IsAbs(path) || path == "." || path == ".." || strings.HasPrefix(path, "../") {
			return Snapshot{}, fmt.Errorf("unsafe skill path %q", file.Path)
		}
		if paths[path] {
			return Snapshot{}, fmt.Errorf("duplicate skill path %q", path)
		}
		paths[path] = true
		if path == "SKILL.md" {
			skillMD = file.Content
		}
	}
	if skillMD == nil {
		return Snapshot{}, errors.New("skill tree is missing SKILL.md")
	}
	meta, warnings, err := ValidateSkillMD(skillMD, expectedName)
	if err != nil {
		return Snapshot{}, err
	}
	for _, file := range files {
		path := filepath.ToSlash(filepath.Clean(file.Path))
		if !strings.HasSuffix(strings.ToLower(path), ".md") || !utf8.Valid(file.Content) {
			continue
		}
		for _, match := range markdownLink.FindAllStringSubmatch(string(file.Content), -1) {
			target := strings.TrimSpace(match[1])
			if fields := strings.Fields(target); len(fields) > 0 {
				target = fields[0]
			}
			target = strings.Trim(target, "<>")
			if fragment := strings.IndexByte(target, '#'); fragment >= 0 {
				target = target[:fragment]
			}
			lower := strings.ToLower(target)
			if target == "" || strings.HasPrefix(target, "#") || strings.Contains(lower, "://") ||
				strings.HasPrefix(lower, "mailto:") || strings.HasPrefix(lower, "data:") {
				continue
			}
			resolved := filepath.ToSlash(filepath.Clean(filepath.Join(filepath.Dir(path), filepath.FromSlash(target))))
			if filepath.IsAbs(resolved) || resolved == ".." || strings.HasPrefix(resolved, "../") {
				return Snapshot{}, fmt.Errorf("Markdown link %q in %s escapes the skill tree", target, path)
			}
			if !paths[resolved] {
				return Snapshot{}, fmt.Errorf("Markdown link %q in %s does not resolve inside the skill tree", target, path)
			}
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return Snapshot{Name: meta.Name, Description: meta.Description, Status: store.SkillActive, Files: files, Warnings: warnings}, nil
}

func LooseDraft(path string) (Snapshot, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return Snapshot{}, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return Snapshot{}, errors.New("loose skill draft must be a regular Markdown file")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return Snapshot{}, err
	}
	if !utf8.Valid(body) || len(body) == 0 || len(body) > 1<<20 {
		return Snapshot{}, errors.New("loose skill draft must be non-empty UTF-8 under 1 MiB")
	}
	name := normalizeName(strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
	if !store.ValidSkillName(name) {
		return Snapshot{}, fmt.Errorf("cannot derive a valid skill name from %q", filepath.Base(path))
	}
	return Snapshot{Name: name, Status: store.SkillDraft,
		SourcePath: path,
		Files:      []store.SkillFileInput{{Path: "SKILL.md", Content: body, Mode: info.Mode()}},
		Warnings:   []string{"imported loose Markdown as an inactive draft; confirm its description with `shale skill activate`"}}, nil
}

func normalizeName(value string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(value) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if r <= unicode.MaxASCII {
				b.WriteRune(r)
				dash = false
			}
			continue
		}
		if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	name := strings.Trim(b.String(), "-")
	if len(name) >= 64 {
		name = strings.TrimRight(name[:63], "-")
	}
	return name
}

func ReplaceSkillMD(files []store.SkillFileInput, replacement []byte) []store.SkillFileInput {
	out := make([]store.SkillFileInput, len(files))
	for i, file := range files {
		out[i] = store.SkillFileInput{Path: file.Path, Content: append([]byte(nil), file.Content...), Mode: file.Mode}
		if filepath.ToSlash(file.Path) == "SKILL.md" {
			out[i].Content = append([]byte(nil), replacement...)
		}
	}
	return out
}

func WrapSkillBody(name, description string, body []byte) []byte {
	description = strings.TrimSpace(description)
	frontmatter := "---\nname: " + name + "\ndescription: " + strconv.Quote(description) + "\n---\n\n"
	return append([]byte(frontmatter), body...)
}
