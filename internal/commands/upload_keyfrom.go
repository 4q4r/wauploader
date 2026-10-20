// upload_keyfrom.go implements the --key-from contract: one key per file,
// rendered from a text/template, so a whole directory tree can be uploaded
// as ONE archive.org item with the tree preserved. It also implements the
// --base-dir root of the relative-path computation and the recursive
// expansion of a directory argument into the files of its tree.
//
// The template is rendered ONCE PER FILE over the file's own data — the
// file name is never re-parsed as a template, so braces in a file name
// are harmless.

package commands

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/template"

	"github.com/4q4r/wauploader/internal/ias3"
	"github.com/4q4r/wauploader/internal/runner"
)

// keyFromData is the template data of one file's key rendering:
//
//	Path — the file's path relative to the base dir, slash-separated;
//	Name — the base filename WITH its extension;
//	Base — the filename WITHOUT its extension;
//	Ext  — the extension without the dot;
//	Dir  — the relative directory part (empty for a top-level file);
//	Abs  — the absolute path of the file.
type keyFromData struct {
	Path string
	Name string
	Base string
	Ext  string
	Dir  string
	Abs  string
}

// keyFromPlan describes a --key-from run for the dry-run review surface:
// the template and the base dir the relative paths come from.
type keyFromPlan struct {
	Template string
	BaseDir  string
}

// uploadKeyFromContract validates the --key-from flag relationships before
// any filesystem or data work: a usage error must never be masked by a
// later error. Every violation is a usage error (exit 2).
func uploadKeyFromContract(f *uploadFlags) error {
	if f.keyFrom == "" {
		if f.baseDir != "" {
			return errors.New("--base-dir допустим только вместе с --key-from: без шаблона ключа нет относительных путей")
		}

		if f.includeHidden {
			return errors.New("--include-hidden допустим только вместе с --key-from: скрытые файлы перечисляются только при раскрытии каталога")
		}

		return nil
	}

	if f.autoMeta {
		return errors.New("--key-from несовместим с --auto-meta: ключи вычисляются из имён файлов")
	}

	if f.identifierScheme != "" {
		return errors.New("--key-from несовместим с --identifier-scheme: схема идентификаторов относится к --auto-meta")
	}

	if f.identifierFrom != "" {
		return errors.New("--key-from несовместим с --identifier-from: идентификатор элемента задаётся --item")
	}

	if f.key != "" {
		return errors.New("--key-from несовместим с --key: ключ вычисляется по шаблону для каждого файла")
	}

	if f.item == "" {
		return errors.New("--key-from требует --item: все файлы дерева загружаются в один элемент")
	}

	return nil
}

// expandUploadDirs expands every directory target into the files of its
// tree, recursively, sorted by the relative path (a deterministic upload
// order — the last file of the item is the one that queues the derive).
// A directory without --key-from is a usage error: the tree's structure
// would be lost in a flat key.
func expandUploadDirs(paths []string, f *uploadFlags) ([]string, error) {
	out := make([]string, 0, len(paths))

	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil || !info.IsDir() {
			// Not a directory (or unreadable): the target stays as it is —
			// a missing file is reported by the runner as a data error,
			// not here as a usage error.
			out = append(out, p)

			continue
		}

		if f.keyFrom == "" {
			if f.autoMeta {
				return nil, fmt.Errorf("каталог %s не поддерживается в режиме --auto-meta: перечислите файлы явно", p)
			}

			return nil, fmt.Errorf("каталог %s требует --key-from для сохранения структуры", p)
		}

		files, err := dirFiles(p, f.includeHidden)
		if err != nil {
			return nil, err
		}

		if len(files) == 0 {
			return nil, fmt.Errorf("каталог %s не содержит файлов (скрытые файлы и каталоги по умолчанию пропускаются; --include-hidden включает их)", p)
		}

		out = append(out, files...)
	}

	return out, nil
}

// dirFiles lists the files of a directory tree, sorted by the relative
// path (slash-separated). Symlinks are not followed: a symlinked
// directory is reported as a target and fails loudly at the upload stage
// instead of being silently expanded.
//
// Hidden entries — a path component starting with "." — are skipped unless
// includeHidden is set: a tree with .git or .env must not be uploaded
// wholesale by accident.
func dirFiles(root string, includeHidden bool) ([]string, error) {
	var rels []string

	err := filepath.WalkDir(root, func(target string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, relErr := filepath.Rel(root, target)
		if relErr != nil {
			return relErr
		}

		slashRel := filepath.ToSlash(rel)

		// The root itself is the operator's explicit choice — only the
		// entries INSIDE the tree are subject to the hidden rule.
		if !includeHidden && slashRel != "." && hasHiddenComponent(slashRel) {
			if entry.IsDir() {
				return filepath.SkipDir
			}

			return nil
		}

		if entry.IsDir() {
			return nil
		}

		rels = append(rels, slashRel)

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("не удалось прочитать каталог %s: %w", root, err)
	}

	sort.Strings(rels)

	files := make([]string, 0, len(rels))
	for _, rel := range rels {
		files = append(files, filepath.Join(root, filepath.FromSlash(rel)))
	}

	return files, nil
}

// hasHiddenComponent reports whether any component of a slash-separated
// relative path starts with "." — a hidden file or directory.
func hasHiddenComponent(rel string) bool {
	for _, segment := range strings.Split(rel, "/") {
		if strings.HasPrefix(segment, ".") {
			return true
		}
	}

	return false
}

// uploadTargetsKeyFrom builds the targets of a --key-from run: one shared
// item identifier (--item) and one key per file, rendered from the
// template. The given targets are the ones the operator passed: the base
// dir comes from them (a directory target contributes its own parent, so
// the tree keeps its directory name in the key) and the files come from
// the recursive expansion. Every violation is a usage error (exit 2).
func uploadTargetsKeyFrom(given []string, f *uploadFlags) ([]runner.UploadTarget, *keyFromPlan, error) {
	tmpl, err := parseKeyTemplate(f.keyFrom)
	if err != nil {
		return nil, nil, err
	}

	if err := ias3.ValidateIdentifier(f.item); err != nil {
		return nil, nil, err
	}

	base, err := keyBaseDir(given, f.baseDir)
	if err != nil {
		return nil, nil, err
	}

	files, err := expandUploadDirs(given, f)
	if err != nil {
		return nil, nil, err
	}

	targets := make([]runner.UploadTarget, 0, len(files))
	seen := make(map[string]string, len(files)) // key → the file that claimed it

	for _, p := range files {
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, nil, errors.New("не удалось определить путь " + p + ": " + err.Error())
		}

		rel, err := filepath.Rel(base, abs)
		if err != nil {
			return nil, nil, errors.New("не удалось вычислить относительный путь для " + p + ": " + err.Error())
		}

		key, err := renderKey(tmpl, p, abs, filepath.ToSlash(rel))
		if err != nil {
			return nil, nil, err
		}

		// Two files with the same key would overwrite each other inside
		// the item — a silent data loss. The same file listed twice is
		// not a collision: it is the same upload.
		if first, ok := seen[key]; ok {
			if first == abs {
				continue
			}

			return nil, nil, fmt.Errorf("ключ «%s» повторяется: %s и %s — второй файл перезаписал бы первый; измените шаблон --key-from", key, first, p)
		}

		seen[key] = abs

		targets = append(targets, runner.UploadTarget{Path: abs, Identifier: f.item, Key: key})
	}

	return targets, &keyFromPlan{Template: f.keyFrom, BaseDir: base}, nil
}

// parseKeyTemplate compiles the --key-from template. Both syntaxes work:
// the placeholder form ("{Path}") and the Go template form ("{{.Path}}").
func parseKeyTemplate(spec string) (*template.Template, error) {
	expanded, err := expandKeyPlaceholders(spec)
	if err != nil {
		return nil, errors.New("неверный шаблон --key-from: " + err.Error())
	}

	tmpl, err := template.New("key").Parse(expanded)
	if err != nil {
		return nil, errors.New("неверный шаблон --key-from: " + err.Error())
	}

	return tmpl, nil
}

// expandKeyPlaceholders rewrites the placeholder form ("{Path}") into the
// Go template form ("{{.Path}}"), so that the documented placeholder
// syntax and the text/template actions share one engine. A Go action
// block is copied verbatim; a placeholder must be closed and must name a
// known field — anything else is a template error.
func expandKeyPlaceholders(spec string) (string, error) {
	var b strings.Builder

	for pos := 0; pos < len(spec); {
		if spec[pos] != '{' {
			b.WriteByte(spec[pos])
			pos++

			continue
		}

		if strings.HasPrefix(spec[pos:], "{{") {
			end := strings.Index(spec[pos+2:], "}}")
			if end < 0 {
				// An unterminated action: the template engine reports it.
				b.WriteString(spec[pos:])

				break
			}

			b.WriteString(spec[pos : pos+2+end+2])
			pos += 2 + end + 2

			continue
		}

		end := strings.IndexByte(spec[pos+1:], '}')
		if end < 0 {
			return "", errors.New("незакрытая подстановка в «" + spec + "»")
		}

		name := spec[pos+1 : pos+1+end]
		if !isKeyFromField(name) {
			return "", errors.New("неизвестное поле шаблона «" + name + "» (доступны: Path, Name, Base, Ext, Dir, Abs)")
		}

		b.WriteString("{{." + name + "}}")

		pos += end + 2
	}

	return b.String(), nil
}

// isKeyFromField reports whether name is one of the documented template
// fields.
func isKeyFromField(name string) bool {
	switch name {
	case "Path", "Name", "Base", "Ext", "Dir", "Abs":
		return true
	default:
		return false
	}
}

// renderKey renders one file's key from the template and validates it:
// an empty key and a key with a "..", "." or empty segment (a traversal
// out of the item, an absolute path, a doubled or trailing separator) are
// usage errors. The key is never normalized — a key that is not a clean
// relative path is refused, not repaired.
func renderKey(tmpl *template.Template, p, abs, rel string) (string, error) {
	name := filepath.Base(abs)
	ext := filepath.Ext(name)

	dir := ""
	if idx := strings.LastIndex(rel, "/"); idx >= 0 {
		dir = rel[:idx]
	}

	data := keyFromData{
		Path: rel,
		Name: name,
		Base: strings.TrimSuffix(name, ext),
		Ext:  strings.TrimPrefix(ext, "."),
		Dir:  dir,
		Abs:  abs,
	}

	var b strings.Builder
	if err := tmpl.Execute(&b, data); err != nil {
		return "", errors.New("не удалось вычислить ключ для " + p + ": " + err.Error())
	}

	key := b.String()
	if key == "" {
		return "", errors.New("пустой ключ для файла " + p + ": шаблон --key-from не дал имени файла")
	}

	for _, segment := range strings.Split(key, "/") {
		switch segment {
		case "..":
			return "", fmt.Errorf("недопустимый ключ «%s» для файла %s: путь не должен содержать «..»", key, p)
		case "", ".":
			return "", fmt.Errorf("недопустимый ключ «%s» для файла %s: пустой сегмент, «.» или ведущий/двойной «/»", key, p)
		}
	}

	return key, nil
}

// keyBaseDir resolves the base dir of the relative paths: the explicit
// --base-dir, or the common parent of the given targets. A directory
// target contributes its OWN parent, so the tree keeps its directory name
// inside the key ("top-taganrog/001.json"). Targets without a common
// parent fall back to the current working directory.
func keyBaseDir(paths []string, baseDirFlag string) (string, error) {
	if baseDirFlag != "" {
		abs, err := filepath.Abs(expandHomePath(baseDirFlag))
		if err != nil {
			return "", errors.New("не удалось определить путь --base-dir " + baseDirFlag + ": " + err.Error())
		}

		info, err := os.Stat(abs)
		if err != nil {
			return "", errors.New("каталог --base-dir " + baseDirFlag + " не найден")
		}

		if !info.IsDir() {
			return "", errors.New("--base-dir " + baseDirFlag + " не является каталогом")
		}

		return abs, nil
	}

	return commonParentDir(paths)
}

// commonParentDir returns the longest common parent directory of the given
// paths, or the current working directory when they have none (different
// roots). Targets under different top-level directories share only the
// leading empty segment: their common parent is the filesystem root.
func commonParentDir(paths []string) (string, error) {
	var common []string

	for idx, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			return "", errors.New("не удалось определить путь " + p + ": " + err.Error())
		}

		// The parent of the target: a file's own directory, a directory's
		// parent (the tree keeps its name in the key).
		segments := strings.Split(filepath.ToSlash(filepath.Dir(abs)), "/")
		if idx == 0 {
			common = segments

			continue
		}

		common = commonSegments(common, segments)
	}

	if len(common) == 0 {
		cwd, err := os.Getwd()
		if err != nil {
			return "", errors.New("не удалось определить текущий каталог: " + err.Error())
		}

		return cwd, nil
	}

	joined := strings.Join(common, "/")
	if joined == "" {
		// Only the leading empty segment is shared: the common parent is
		// the filesystem root. An empty base dir would break the relative
		// path computation with a confusing error.
		joined = "/"
	}

	return filepath.FromSlash(joined), nil
}

// commonSegments returns the longest common prefix of two path-segment
// lists.
func commonSegments(first, second []string) []string {
	limit := len(first)
	if len(second) < limit {
		limit = len(second)
	}

	common := make([]string, 0, limit)
	for idx := 0; idx < limit; idx++ {
		if first[idx] != second[idx] {
			break
		}

		common = append(common, first[idx])
	}

	return common
}
