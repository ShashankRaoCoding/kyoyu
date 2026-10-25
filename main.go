// kyoyu turns a prebuilt binary into a pip-installable package and uploads it.
//
//	kyoyu -bin ./mycli
//
// It infers the project name from the binary's file name, generates a Python
// project (pyproject.toml, setup.py, launcher shim, README) around it, builds a
// platform-tagged wheel, and uploads that wheel with twine.
package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"compress/flate"
	"crypto/sha256"
	"debug/elf"
	"debug/macho"
	"debug/pe"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

type config struct {
	bin      string
	name     string
	version  string
	project  string
	plat     string
	desc     string
	repo     string
	noUpload bool
	yes      bool
	force    bool
	verbose  bool
}

var nameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "kyoyu:", err)
		os.Exit(1)
	}
}

func parseArgs(args []string) (*config, error) {
	cfg := &config{}
	fs := flag.NewFlagSet("kyoyu", flag.ContinueOnError)
	fs.StringVar(&cfg.bin, "bin", "", "path to the compiled binary (required)")
	fs.StringVar(&cfg.name, "name", "", "package name on PyPI (default: the binary's file name)")
	fs.StringVar(&cfg.version, "version", "", "release version (default: 0.1.0, or the existing project's)")
	fs.StringVar(&cfg.project, "project", "", "project directory to create (default: ./<name>-pypi)")
	fs.StringVar(&cfg.plat, "plat", "", "wheel platform tag (default: detected from the binary)")
	fs.StringVar(&cfg.desc, "description", "", "one-line package description")
	fs.StringVar(&cfg.repo, "repository", "", "twine repository: a .pypirc name such as testpypi, or an upload URL")
	fs.BoolVar(&cfg.noUpload, "no-upload", false, "build the wheel but do not upload it")
	fs.BoolVar(&cfg.yes, "y", false, "skip the upload confirmation prompt")
	fs.BoolVar(&cfg.verbose, "verbose", false, "print each step and run twine with --verbose")
	fs.BoolVar(&cfg.force, "force", false, "overwrite existing generated project files")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: kyoyu -bin <binary> [flags]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return nil, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if cfg.bin == "" {
		fs.Usage()
		return nil, fmt.Errorf("-bin is required")
	}
	return cfg, nil
}

func run(args []string) error {
	cfg, err := parseArgs(args)
	if err != nil {
		return err
	}

	st, err := os.Stat(cfg.bin)
	if err != nil {
		return err
	}
	if st.IsDir() {
		return fmt.Errorf("%s is a directory, expected a binary file", cfg.bin)
	}

	// The command users will type is the binary's own name.
	script := strings.TrimSuffix(filepath.Base(cfg.bin), ".exe")
	if !nameRe.MatchString(script) {
		return fmt.Errorf("cannot use %q as a command name (letters, digits, - and _ only, starting with a letter); rename the binary", script)
	}
	dist := cfg.name
	if dist == "" {
		dist = script
	}
	if !nameRe.MatchString(dist) {
		return fmt.Errorf("invalid package name %q", dist)
	}
	pkg := normalize(dist)
	project := cfg.project
	if project == "" {
		// Not just <name>: the binary usually sits at ./<name> already.
		project = dist + "-pypi"
	}
	if st, err := os.Stat(project); err == nil && !st.IsDir() {
		return fmt.Errorf("%s exists and is not a directory; pass -project to choose another location", project)
	}

	// Pick the platform before touching the disk so a bad -plat fails early.
	plat := cfg.plat
	if plat == "" {
		detected, derr := detectPlatform(cfg.bin)
		if derr != nil {
			host, herr := hostPlatform()
			if herr != nil {
				return fmt.Errorf("%v (and %v); pass -plat", derr, herr)
			}
			fmt.Fprintf(os.Stderr, "warning: %v; assuming this machine (%s). Pass -plat to override.\n", derr, host)
			detected = host
		}
		plat = detected
	}

	cfg.logf("binary:   %s (%d bytes)", cfg.bin, st.Size())
	cfg.logf("name:     %s (command %q, module %q)", dist, script, pkg)
	cfg.logf("platform: %s", plat)
	created, err := scaffold(cfg, project, dist, pkg, script)
	if err != nil {
		return err
	}
	if created {
		fmt.Printf("created project %s/\n", project)
	} else {
		fmt.Printf("updated existing project %s/\n", project)
	}

	wheel, err := buildWheel(project, filepath.Join(project, "dist"), plat)
	if err != nil {
		return err
	}
	fmt.Println("built", wheel)

	if cfg.noUpload {
		return nil
	}
	meta := parseProject(readFile(filepath.Join(project, "pyproject.toml")))
	return upload(wheel, meta.fields["name"], meta.fields["version"], cfg)
}

func normalize(name string) string {
	return strings.ToLower(regexp.MustCompile(`[-_.]+`).ReplaceAllString(name, "_"))
}

func confirm(prompt string) bool {
	fmt.Print(prompt + " [y/N] ")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes"
}

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func readFile(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}

// logf prints progress detail only when -verbose is set.
func (c *config) logf(format string, a ...any) {
	if c.verbose {
		fmt.Fprintf(os.Stderr, "kyoyu: "+format+"\n", a...)
	}
}

const pyprojectTmpl = `[build-system]
requires = ["setuptools>=68", "wheel"]
build-backend = "setuptools.build_meta"

[project]
name = "{{DIST}}"
version = "{{VERSION}}"
description = "{{DESC}}"
readme = "README.md"
requires-python = ">=3.8"

[project.scripts]
{{SCRIPT}} = "{{PKG}}.__main__:main"

[tool.setuptools.packages.find]
where = ["src"]

[tool.setuptools.package-data]
{{PKG}} = ["bin/*"]
`

const setupTmpl = `from setuptools import setup
from wheel.bdist_wheel import bdist_wheel


class PlatformWheel(bdist_wheel):
    """Tag the wheel as platform-specific, but independent of the Python ABI."""

    def finalize_options(self):
        super().finalize_options()
        self.root_is_pure = False

    def get_tag(self):
        _, _, plat = super().get_tag()
        return "py3", "none", plat


setup(cmdclass={"bdist_wheel": PlatformWheel})
`

const shimTmpl = `"""Launcher that runs the bundled {{SCRIPT}} binary."""
import os
import stat
import subprocess
import sys
from pathlib import Path

NAME = "{{SCRIPT}}"


def _binary_path():
    bin_dir = Path(__file__).parent / "bin"
    for candidate in (NAME, NAME + ".exe"):
        path = bin_dir / candidate
        if path.exists():
            return path
    sys.exit(f"{NAME}: bundled binary not found in {bin_dir}")


def main():
    exe = _binary_path()
    if os.name == "nt":
        sys.exit(subprocess.call([str(exe), *sys.argv[1:]]))
    # pip may strip the executable bit; restore it before exec.
    exe.chmod(exe.stat().st_mode | stat.S_IXUSR | stat.S_IXGRP | stat.S_IXOTH)
    os.execv(exe, [str(exe), *sys.argv[1:]])


if __name__ == "__main__":
    main()
`

const readmeTmpl = `# {{DIST}}

{{DESC}}

` + "```bash\npip install {{DIST}}\n{{SCRIPT}} --help\n```" + `
`

const gitignore = "dist/\nbuild/\n*.egg-info/\n__pycache__/\n"

func fill(tmpl string, vars map[string]string) string {
	for k, v := range vars {
		tmpl = strings.ReplaceAll(tmpl, "{{"+k+"}}", v)
	}
	return tmpl
}

// scaffold creates (or refreshes) the Python project around the binary. It
// reports whether the project was newly created. Existing generated files are
// left alone unless -force is given, except the bundled binary, which is always
// replaced, and the version, which follows -version when one is passed.
func scaffold(cfg *config, project, dist, pkg, script string) (bool, error) {
	pyprojectPath := filepath.Join(project, "pyproject.toml")
	_, statErr := os.Stat(pyprojectPath)
	created := os.IsNotExist(statErr)

	version := cfg.version
	if version == "" {
		version = "0.1.0"
		if !created {
			if body, err := os.ReadFile(pyprojectPath); err == nil {
				if v := parseProject(string(body)).fields["version"]; v != "" {
					version = v
				}
			}
		}
	}
	desc := cfg.desc
	if desc == "" {
		desc = dist + ", distributed as a prebuilt binary"
	}

	vars := map[string]string{
		"DIST": dist, "PKG": pkg, "SCRIPT": script,
		"VERSION": version, "DESC": strings.ReplaceAll(desc, `"`, `'`),
	}
	pkgDir := filepath.Join(project, "src", pkg)
	binDir := filepath.Join(pkgDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return false, err
	}

	files := []struct{ path, body string }{
		{pyprojectPath, fill(pyprojectTmpl, vars)},
		{filepath.Join(project, "setup.py"), setupTmpl},
		{filepath.Join(project, "README.md"), fill(readmeTmpl, vars)},
		{filepath.Join(project, ".gitignore"), gitignore},
		{filepath.Join(pkgDir, "__init__.py"), ""},
		{filepath.Join(pkgDir, "__main__.py"), fill(shimTmpl, vars)},
	}
	for _, f := range files {
		if _, err := os.Stat(f.path); err == nil && !cfg.force {
			continue
		}
		if err := os.WriteFile(f.path, []byte(f.body), 0o644); err != nil {
			return false, err
		}
	}

	if !created && cfg.version != "" {
		if err := setVersion(pyprojectPath, cfg.version); err != nil {
			return false, err
		}
	}

	return created, installBinary(cfg.bin, binDir, filepath.Base(cfg.bin))
}

var versionLineRe = regexp.MustCompile(`(?m)^version\s*=\s*"[^"]*"`)

func setVersion(path, version string) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	done := false
	out := versionLineRe.ReplaceAllStringFunc(string(body), func(string) string {
		if done {
			return `version = "` + version + `"`
		}
		done = true
		return `version = "` + version + `"`
	})
	if !done {
		return fmt.Errorf("no version line found in %s", path)
	}
	return os.WriteFile(path, []byte(out), 0o644)
}

// installBinary replaces whatever is in bin/ with the given binary, so a stale
// binary for another platform never ships by accident.
func installBinary(src, binDir, name string) error {
	entries, err := os.ReadDir(binDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			if err := os.Remove(filepath.Join(binDir, e.Name())); err != nil {
				return err
			}
		}
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(filepath.Join(binDir, name), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

type entry struct {
	path string // path inside the wheel
	body []byte
	mode fs.FileMode
}

type projectMeta struct {
	fields  map[string]string
	scripts map[string]string
}

var kvRe = regexp.MustCompile(`^\s*([A-Za-z0-9_.-]+)\s*=\s*"([^"]*)"\s*(#.*)?$`)

// parseProject is a small reader for the string-valued keys in [project] and
// [project.scripts]; it is not a general TOML parser.
func parseProject(src string) projectMeta {
	m := projectMeta{fields: map[string]string{}, scripts: map[string]string{}}
	section := ""
	for _, line := range strings.Split(src, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
			section = strings.Trim(t, "[] ")
			continue
		}
		kv := kvRe.FindStringSubmatch(line)
		if kv == nil {
			continue
		}
		switch section {
		case "project":
			m.fields[kv[1]] = kv[2]
		case "project.scripts":
			m.scripts[kv[1]] = kv[2]
		}
	}
	return m
}

// buildWheel writes <outDir>/<pkg>-<version>-py3-none-<plat>.whl from the
// project on disk, without needing Python.
func buildWheel(project, outDir, plat string) (string, error) {
	toml, err := os.ReadFile(filepath.Join(project, "pyproject.toml"))
	if err != nil {
		return "", err
	}
	meta := parseProject(string(toml))
	name, version := meta.fields["name"], meta.fields["version"]
	if name == "" || version == "" {
		return "", fmt.Errorf("pyproject.toml is missing name or version")
	}
	pkg := normalize(name)
	pkgDir := filepath.Join(project, "src", pkg)

	var entries []entry
	err = filepath.WalkDir(pkgDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "__pycache__" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") || strings.HasSuffix(d.Name(), ".pyc") {
			return nil
		}
		rel, _ := filepath.Rel(pkgDir, p)
		rel = filepath.ToSlash(rel)
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		mode := fs.FileMode(0o644)
		if strings.HasPrefix(rel, "bin/") {
			mode = 0o755
		}
		entries = append(entries, entry{pkg + "/" + rel, body, mode})
		return nil
	})
	if err != nil {
		return "", err
	}
	hasBin := false
	for _, e := range entries {
		if strings.HasPrefix(e.path, pkg+"/bin/") {
			hasBin = true
		}
	}
	if !hasBin {
		return "", fmt.Errorf("no binary found in %s", filepath.Join(pkgDir, "bin"))
	}

	distInfo := fmt.Sprintf("%s-%s.dist-info", pkg, version)
	entries = append(entries,
		entry{distInfo + "/METADATA", []byte(metadataText(name, version, meta, project)), 0o644},
		entry{distInfo + "/WHEEL", []byte("Wheel-Version: 1.0\nGenerator: kyoyu (go)\nRoot-Is-Purelib: false\nTag: py3-none-" + plat + "\n"), 0o644},
		entry{distInfo + "/top_level.txt", []byte(pkg + "\n"), 0o644},
	)
	if len(meta.scripts) > 0 {
		keys := make([]string, 0, len(meta.scripts))
		for k := range meta.scripts {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var sb strings.Builder
		sb.WriteString("[console_scripts]\n")
		for _, k := range keys {
			fmt.Fprintf(&sb, "%s = %s\n", k, meta.scripts[k])
		}
		entries = append(entries, entry{distInfo + "/entry_points.txt", []byte(sb.String()), 0o644})
	}

	var rec bytes.Buffer
	w := csv.NewWriter(&rec)
	for _, e := range entries {
		sum := sha256.Sum256(e.body)
		_ = w.Write([]string{e.path, "sha256=" + base64.RawURLEncoding.EncodeToString(sum[:]), fmt.Sprint(len(e.body))})
	}
	_ = w.Write([]string{distInfo + "/RECORD", "", ""})
	w.Flush()
	entries = append(entries, entry{distInfo + "/RECORD", rec.Bytes(), 0o644})

	// Remove wheels of the same name from earlier runs so dist/ only holds
	// what this run built.
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", err
	}
	out := filepath.Join(outDir, fmt.Sprintf("%s-%s-py3-none-%s.whl", pkg, version, plat))
	if err := writeZip(out, entries); err != nil {
		return "", err
	}
	return out, nil
}

func metadataText(name, version string, meta projectMeta, project string) string {
	var sb strings.Builder
	sb.WriteString("Metadata-Version: 2.1\n")
	fmt.Fprintf(&sb, "Name: %s\nVersion: %s\n", name, version)
	if d := meta.fields["description"]; d != "" {
		fmt.Fprintf(&sb, "Summary: %s\n", d)
	}
	if r := meta.fields["requires-python"]; r != "" {
		fmt.Fprintf(&sb, "Requires-Python: %s\n", r)
	}
	if readme := meta.fields["readme"]; readme != "" {
		if body, err := os.ReadFile(filepath.Join(project, readme)); err == nil {
			ct := "text/plain"
			if strings.HasSuffix(strings.ToLower(readme), ".md") {
				ct = "text/markdown"
			}
			fmt.Fprintf(&sb, "Description-Content-Type: %s\n\n%s", ct, body)
		}
	}
	return sb.String()
}

// writeZip writes the wheel with every entry's CRC and sizes stored in its
// local header. archive/zip's streaming CreateHeader sets the "data descriptor"
// flag instead, and PyPI rejects archives containing data descriptors.
func writeZip(path string, entries []entry) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	stamp := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC) // reproducible output
	for _, e := range entries {
		var packed bytes.Buffer
		fw, err := flate.NewWriter(&packed, flate.BestCompression)
		if err != nil {
			return err
		}
		if _, err := fw.Write(e.body); err != nil {
			return err
		}
		if err := fw.Close(); err != nil {
			return err
		}
		h := &zip.FileHeader{
			Name:               e.path,
			Method:             zip.Deflate,
			Modified:           stamp,
			CRC32:              crc32.ChecksumIEEE(e.body),
			CompressedSize64:   uint64(packed.Len()),
			UncompressedSize64: uint64(len(e.body)),
		}
		h.SetMode(e.mode)
		raw, err := zw.CreateRaw(h)
		if err != nil {
			return err
		}
		if _, err := raw.Write(packed.Bytes()); err != nil {
			return err
		}
	}
	return zw.Close()
}

// detectPlatform reads the binary's own header to choose the wheel tag, so a
// cross-compiled binary is tagged for its target rather than for this machine.
func detectPlatform(path string) (string, error) {
	if f, err := elf.Open(path); err == nil {
		defer f.Close()
		if f.OSABI != elf.ELFOSABI_NONE && f.OSABI != elf.ELFOSABI_LINUX {
			return "", fmt.Errorf("ELF binary is not for Linux (OS/ABI %v)", f.OSABI)
		}
		arch, ok := map[elf.Machine]string{
			elf.EM_X86_64: "x86_64", elf.EM_AARCH64: "aarch64",
			elf.EM_386: "i686", elf.EM_ARM: "armv7l",
		}[f.Machine]
		if !ok {
			return "", fmt.Errorf("unsupported ELF machine %v", f.Machine)
		}
		if s := f.Section(".interp"); s != nil {
			if data, err := s.Data(); err == nil && bytes.Contains(data, []byte("musl")) {
				return "musllinux_1_2_" + arch, nil
			}
		}
		return "manylinux_2_17_" + arch, nil
	}
	if f, err := macho.Open(path); err == nil {
		defer f.Close()
		switch f.Cpu {
		case macho.CpuArm64:
			return "macosx_11_0_arm64", nil
		case macho.CpuAmd64:
			return "macosx_10_9_x86_64", nil
		}
		return "", fmt.Errorf("unsupported Mach-O CPU %v", f.Cpu)
	}
	if ff, err := macho.OpenFat(path); err == nil {
		defer ff.Close()
		var arm, amd bool
		for _, a := range ff.Arches {
			arm = arm || a.Cpu == macho.CpuArm64
			amd = amd || a.Cpu == macho.CpuAmd64
		}
		if arm && amd {
			return "macosx_10_9_universal2", nil
		}
		return "", fmt.Errorf("universal Mach-O binary lacks an arm64 or x86_64 slice")
	}
	if f, err := pe.Open(path); err == nil {
		defer f.Close()
		switch f.Machine {
		case pe.IMAGE_FILE_MACHINE_AMD64:
			return "win_amd64", nil
		case pe.IMAGE_FILE_MACHINE_ARM64:
			return "win_arm64", nil
		case pe.IMAGE_FILE_MACHINE_I386:
			return "win32", nil
		}
		return "", fmt.Errorf("unsupported PE machine 0x%x", f.Machine)
	}
	return "", fmt.Errorf("%s is not a recognised native executable (ELF, Mach-O or PE)", filepath.Base(path))
}

func hostPlatform() (string, error) {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "linux/amd64":
		return "manylinux_2_17_x86_64", nil
	case "linux/arm64":
		return "manylinux_2_17_aarch64", nil
	case "darwin/arm64":
		return "macosx_11_0_arm64", nil
	case "darwin/amd64":
		return "macosx_10_9_x86_64", nil
	case "windows/amd64":
		return "win_amd64", nil
	}
	return "", fmt.Errorf("no default platform tag for %s/%s", runtime.GOOS, runtime.GOARCH)
}

// findTwine returns the command prefix that runs twine on this machine.
func findTwine() ([]string, error) {
	candidates := [][]string{{"twine"}, {"python3", "-m", "twine"}, {"python", "-m", "twine"}}
	for _, c := range candidates {
		if _, err := exec.LookPath(c[0]); err != nil {
			continue
		}
		probe := exec.Command(c[0], append(append([]string{}, c[1:]...), "--version")...)
		if err := probe.Run(); err == nil {
			return c, nil
		}
	}
	return nil, fmt.Errorf("twine not found; install it with: pip install twine")
}

func runTwine(cfg *config, tw []string, args ...string) error {
	cfg.logf("run: %s", strings.Join(append(append([]string{}, tw...), args...), " "))
	cmd := exec.Command(tw[0], append(append([]string{}, tw[1:]...), args...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// upload validates the wheel with `twine check`, asks for confirmation when run
// from a terminal, then uploads exactly this wheel (never dist/*, so stale
// files from earlier releases are not re-sent).
func upload(wheel, name, version string, cfg *config) error {
	tw, err := findTwine()
	if err != nil {
		return fmt.Errorf("%w (wheel is built at %s)", err, wheel)
	}
	if err := runTwine(cfg, tw, "check", "--strict", wheel); err != nil {
		return fmt.Errorf("twine check failed: %w", err)
	}
	cfg.logf("checking %s for %q %s", target0(cfg.repo), name, version)
	if err := preflight(name, version, cfg.repo); err != nil {
		return fmt.Errorf("%w (wheel is built at %s)", err, wheel)
	}

	target := "PyPI"
	if cfg.repo != "" {
		target = cfg.repo
	}
	if !cfg.yes && isTerminal(os.Stdin) {
		if !confirm(fmt.Sprintf("Upload %s to %s? Uploads cannot be undone or overwritten.", filepath.Base(wheel), target)) {
			fmt.Println("not uploaded; wheel left at", wheel)
			return nil
		}
	}

	args := []string{"upload"}
	if cfg.verbose {
		args = append(args, "--verbose")
	}
	switch {
	case cfg.repo == "":
	case strings.HasPrefix(cfg.repo, "http://"), strings.HasPrefix(cfg.repo, "https://"):
		args = append(args, "--repository-url", cfg.repo)
	default:
		args = append(args, "--repository", cfg.repo)
	}
	args = append(args, wheel)
	if err := runTwine(cfg, tw, args...); err != nil {
		if !cfg.verbose {
			return fmt.Errorf("twine upload failed: %w (rerun with -verbose for PyPI's full response)", err)
		}
		return fmt.Errorf("twine upload failed: %w", err)
	}
	return nil
}

// indexBase maps -repository to the web index used for the pre-upload lookup.
// An empty result means "unknown index": the lookup is skipped.
func indexBase(repo string) string {
	switch {
	case repo == "" || repo == "pypi":
		return "https://pypi.org"
	case repo == "testpypi":
		return "https://test.pypi.org"
	}
	return ""
}

var indexOverride string // set by tests

// preflight asks the index whether the name exists and whether this exact
// version was already released, because PyPI answers both with a terse 400/403
// after the upload. Network problems only produce a warning.
func preflight(name, version, repo string) error {
	base := indexBase(repo)
	if indexOverride != "" {
		base = indexOverride
	}
	if base == "" {
		return nil
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(base + "/pypi/" + name + "/json")
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not check %s for %q: %v\n", base, name, err)
		return nil
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotFound:
		fmt.Printf("%q is not taken on %s\n", name, base)
		return nil
	case http.StatusOK:
	default:
		fmt.Fprintf(os.Stderr, "warning: %s returned %s when looking up %q; continuing\n", base, resp.Status, name)
		return nil
	}
	var info struct {
		Releases map[string][]json.RawMessage `json:"releases"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not read the lookup for %q: %v\n", name, err)
		return nil
	}
	if files := info.Releases[version]; len(files) > 0 {
		return fmt.Errorf("%s %s is already on %s; versions cannot be re-uploaded, even after deleting. Pass -version with a new number", name, version, base)
	}
	fmt.Fprintf(os.Stderr, "warning: %q already exists on %s. The upload only succeeds if you own that project; otherwise pick another name with -name\n", name, base)
	return nil
}

func target0(repo string) string {
	if b := indexBase(repo); b != "" {
		return b
	}
	return repo
}
