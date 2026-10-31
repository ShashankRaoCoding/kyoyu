package main

// Installer mode (-github owner/repo): instead of bundling the binary in a
// platform wheel, publish one pure-Python wheel whose launcher downloads the
// right binary from that repository's GitHub Releases on first run. The PyPI
// package then never changes when the program does: a new release is just a
// new GitHub Release.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	defaultAsset     = "{script}_{os}_{arch}{ext}"
	defaultChecksums = "SHA256SUMS"
)

var (
	repoRe     = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	assetRe    = regexp.MustCompile(`^[A-Za-z0-9_.{}-]+$`)
	sumsNameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	// Everything left after removing the placeholders we fill in.
	placeholderRe = regexp.MustCompile(`\{(script|os|arch|ext)\}`)
)

// checkInstallerFlags validates everything that ends up inside the generated
// Python source, so a flag value can never inject code into the launcher.
func checkInstallerFlags(cfg *config) error {
	if !repoRe.MatchString(cfg.github) {
		return fmt.Errorf("-github %q must look like owner/repo", cfg.github)
	}
	if cfg.plat != "" {
		return fmt.Errorf("-plat does not apply with -github: the installer wheel works on every platform")
	}
	if cfg.name == "" && cfg.command == "" && cfg.bin == "" {
		return fmt.Errorf("-github needs -name (the PyPI package name), and optionally -command")
	}
	if !assetRe.MatchString(cfg.asset) || strings.ContainsAny(placeholderRe.ReplaceAllString(cfg.asset, ""), "{}") {
		return fmt.Errorf("-asset %q may only use letters, digits, . _ - and the placeholders {script} {os} {arch} {ext}", cfg.asset)
	}
	if !sumsNameRe.MatchString(cfg.checksums) {
		return fmt.Errorf("-checksums %q is not a plain file name", cfg.checksums)
	}
	return nil
}

func runInstaller(cfg *config) error {
	script := cfg.command
	if script == "" && cfg.bin != "" {
		script = strings.TrimSuffix(filepath.Base(cfg.bin), ".exe")
	}
	if script == "" {
		script = cfg.name
	}
	if !nameRe.MatchString(script) {
		return fmt.Errorf("cannot use %q as a command name (letters, digits, - and _ only, starting with a letter); pass -command", script)
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
		project = dist + "-pypi"
	}
	if st, err := os.Stat(project); err == nil && !st.IsDir() {
		return fmt.Errorf("%s exists and is not a directory; pass -project to choose another location", project)
	}

	cfg.logf("installer: %s -> github.com/%s (asset %q, sums %q)", dist, cfg.github, cfg.asset, cfg.checksums)
	cfg.logf("name:      %s (command %q, module %q)", dist, script, pkg)
	created, err := scaffoldInstaller(cfg, project, dist, pkg, script)
	if err != nil {
		return err
	}
	if created {
		fmt.Printf("created project %s/\n", project)
	} else {
		fmt.Printf("updated existing project %s/\n", project)
	}

	wheel, err := buildWheel(project, filepath.Join(project, "dist"), "any")
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

// scaffoldInstaller mirrors scaffold, minus the bundled binary.
func scaffoldInstaller(cfg *config, project, dist, pkg, script string) (bool, error) {
	pyprojectPath := filepath.Join(project, "pyproject.toml")
	_, statErr := os.Stat(pyprojectPath)
	created := os.IsNotExist(statErr)

	desc := cfg.desc
	if desc == "" {
		desc = fmt.Sprintf("Installer for %s: fetches the prebuilt program from GitHub Releases", dist)
	}
	vars := map[string]string{
		"DIST": dist, "PKG": pkg, "SCRIPT": script,
		"VERSION":   pickVersion(cfg, pyprojectPath, created),
		"DESC":      strings.ReplaceAll(desc, `"`, `'`),
		"REPO":      cfg.github,
		"ASSET":     cfg.asset,
		"CHECKSUMS": cfg.checksums,
		"ENVVAR":    strings.ToUpper(pkg) + "_VERSION",
	}
	pkgDir := filepath.Join(project, "src", pkg)
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		return false, err
	}

	files := []struct{ path, body string }{
		{pyprojectPath, fill(installerPyprojectTmpl, vars)},
		{filepath.Join(project, "setup.py"), installerSetup},
		{filepath.Join(project, "README.md"), fill(installerReadme(), vars)},
		{filepath.Join(project, ".gitignore"), gitignore},
		{filepath.Join(pkgDir, "__init__.py"), ""},
		{filepath.Join(pkgDir, "__main__.py"), fill(launcherTmpl, vars)},
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
	return created, nil
}

const installerPyprojectTmpl = `[build-system]
requires = ["setuptools>=68", "wheel"]
build-backend = "setuptools.build_meta"

[project]
name = "{{DIST}}"
version = "{{VERSION}}"
description = "{{DESC}}"
readme = "README.md"
requires-python = ">=3.8"

[project.urls]
Source = "https://github.com/{{REPO}}"

[project.scripts]
{{SCRIPT}} = "{{PKG}}.__main__:main"

[tool.setuptools.packages.find]
where = ["src"]
`

const installerSetup = "from setuptools import setup\n\nsetup()\n"

// installerReadme is built with a fence helper because a Go raw string cannot
// contain backticks.
func installerReadme() string {
	f := "```"
	return "# {{DIST}}\n\n{{DESC}}\n\n" +
		f + "bash\npip install {{DIST}}\n{{SCRIPT}} --help\n" + f + "\n\n" +
		"## How it works\n\n" +
		"This package contains only a small launcher. The first time you run it, the launcher\n" +
		"downloads the {{SCRIPT}} program for your platform from the GitHub Releases of\n" +
		"https://github.com/{{REPO}}, checks it against the SHA-256 sums published in the\n" +
		"same release ({{CHECKSUMS}}), and keeps it in your user cache directory. It looks\n" +
		"for a newer release at most once a day and keeps working offline once a version is cached.\n\n" +
		"Run a specific release:\n\n" +
		f + "bash\n{{ENVVAR}}=1.2.3 {{SCRIPT}} --help\n" + f + "\n\n" +
		"## Release assets\n\n" +
		"Each GitHub Release must contain one file per platform named {{ASSET}}\n" +
		"(os is linux, darwin or windows; arch is amd64, arm64, 386 or arm; ext is .exe on Windows)\n" +
		"and a {{CHECKSUMS}} file in sha256sum format.\n"
}

// launcherTmpl is the whole runtime of an installer package. It uses only the
// standard library and must not contain backticks (it lives in a Go raw string).
const launcherTmpl = `"""Launcher for {{SCRIPT}}.

The {{SCRIPT}} program is not stored in this package. On first use it is
downloaded from https://github.com/{{REPO}}/releases, checked against the
SHA-256 sums published in the same release, and cached in your user cache
directory.
"""
import hashlib
import json
import os
import platform
import re
import stat
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

NAME = "{{SCRIPT}}"
DIST = "{{DIST}}"
REPO = "{{REPO}}"
ASSET = "{{ASSET}}"
CHECKSUMS = "{{CHECKSUMS}}"
VERSION_ENV = "{{ENVVAR}}"
CHECK_EVERY = 24 * 60 * 60  # seconds between looks for a newer release
TIMEOUT = 15
USER_AGENT = "kyoyu-launcher"
TAG_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._+-]*$")


def _fail(message):
    sys.exit(f"{NAME}: {message}")


def _target():
    """The (os, arch, ext) names used in release asset file names."""
    if sys.platform.startswith("linux"):
        os_name = "linux"
    elif sys.platform == "darwin":
        os_name = "darwin"
    elif sys.platform in ("win32", "cygwin", "msys"):
        os_name = "windows"
    else:
        _fail(f"no prebuilt program for platform {sys.platform!r}")
    arches = {
        "x86_64": "amd64", "amd64": "amd64",
        "aarch64": "arm64", "arm64": "arm64",
        "i386": "386", "i686": "386", "x86": "386",
        "armv7l": "arm", "armv6l": "arm",
    }
    machine = platform.machine().lower()
    arch = arches.get(machine)
    if arch is None:
        _fail(f"no prebuilt program for CPU {machine!r}")
    return os_name, arch, (".exe" if os_name == "windows" else "")


def _asset_name(os_name, arch, ext):
    name = ASSET
    for key, value in (("{script}", NAME), ("{os}", os_name), ("{arch}", arch), ("{ext}", ext)):
        name = name.replace(key, value)
    return name


def _cache_root():
    if os.name == "nt":
        base = os.environ.get("LOCALAPPDATA") or str(Path.home() / "AppData" / "Local")
    elif sys.platform == "darwin":
        base = str(Path.home() / "Library" / "Caches")
    else:
        base = os.environ.get("XDG_CACHE_HOME") or str(Path.home() / ".cache")
    return Path(base) / DIST


def _open(url, method="GET"):
    request = urllib.request.Request(url, method=method, headers={"User-Agent": USER_AGENT})
    return urllib.request.urlopen(request, timeout=TIMEOUT)


def _release_url(tag, filename):
    return f"https://github.com/{REPO}/releases/download/{urllib.parse.quote(tag)}/{urllib.parse.quote(filename)}"


def _latest_tag():
    # The /releases/latest page redirects to /releases/tag/<tag>. Reading the
    # redirect avoids the API and its unauthenticated rate limit.
    with _open(f"https://github.com/{REPO}/releases/latest", "HEAD") as response:
        final = response.geturl()
    marker = "/releases/tag/"
    if marker not in final:
        raise RuntimeError(f"{REPO} has no releases")
    return urllib.parse.unquote(final.split(marker, 1)[1]).strip("/")


def _candidates(version):
    """Tags to try for a version: as given, then with the v prefix toggled."""
    version = version.strip()
    if not TAG_RE.match(version):
        _fail(f"{version!r} is not a valid version")
    other = version[1:] if version.startswith("v") else "v" + version
    return [version, other] if TAG_RE.match(other) else [version]


def _expected_sum(tag, asset):
    with _open(_release_url(tag, CHECKSUMS)) as response:
        text = response.read().decode("utf-8", "replace")
    for line in text.splitlines():
        parts = line.split()
        if len(parts) == 2 and parts[1].lstrip("*") == asset:
            return parts[0].lower()
    _fail(f"{CHECKSUMS} of release {tag} has no entry for {asset}")


def _download(tag, asset, dest):
    wanted = _expected_sum(tag, asset)
    dest.parent.mkdir(parents=True, exist_ok=True)
    handle, tmp = tempfile.mkstemp(dir=str(dest.parent), prefix=".download-")
    try:
        digest = hashlib.sha256()
        with os.fdopen(handle, "wb") as out, _open(_release_url(tag, asset)) as response:
            while True:
                chunk = response.read(1 << 20)
                if not chunk:
                    break
                digest.update(chunk)
                out.write(chunk)
        if digest.hexdigest() != wanted:
            _fail(f"checksum mismatch for {asset} {tag}; refusing to run it")
        os.chmod(tmp, 0o755)
        os.replace(tmp, dest)
    finally:
        if os.path.exists(tmp):
            os.unlink(tmp)


def _install(version):
    """Return (tag, path) of the program for this version, downloading it if needed."""
    os_name, arch, ext = _target()
    asset = _asset_name(os_name, arch, ext)
    root = _cache_root()
    tags = _candidates(version)
    for tag in tags:
        path = root / "versions" / tag / (NAME + ext)
        if path.is_file():
            return tag, path
    for tag in tags:
        path = root / "versions" / tag / (NAME + ext)
        try:
            print(f"{NAME}: downloading {asset} {tag} from github.com/{REPO}", file=sys.stderr)
            _download(tag, asset, path)
            return tag, path
        except urllib.error.HTTPError as err:
            if err.code != 404:
                _fail(f"download failed: HTTP {err.code} {err.reason} for {err.url}")
        except (urllib.error.URLError, OSError) as err:
            _fail(f"could not download {asset} {tag}: {err}")
    _fail(f"release {version} of {REPO} not found, or it has no {asset} / {CHECKSUMS}")


def _load_state(path):
    try:
        state = json.loads(path.read_text())
        tag, checked = state["tag"], float(state["checked"])
    except (OSError, ValueError, KeyError, TypeError):
        return None, 0.0
    return (tag if isinstance(tag, str) and TAG_RE.match(tag) else None), checked


def _resolve():
    """Pick the version to run: the pinned one, else the newest release."""
    pinned = os.environ.get(VERSION_ENV, "").strip()
    if pinned:
        return _install(pinned)[1]
    state_path = _cache_root() / "latest.json"
    tag, checked = _load_state(state_path)
    if tag is None or time.time() - checked > CHECK_EVERY:
        try:
            tag = _latest_tag()
            if not TAG_RE.match(tag):
                raise RuntimeError(f"unexpected release tag {tag!r}")
            state_path.parent.mkdir(parents=True, exist_ok=True)
            state_path.write_text(json.dumps({"tag": tag, "checked": time.time()}))
        except Exception as err:  # offline, rate limited, no releases...
            if tag is None:
                _fail(f"could not look up the latest release of {REPO}: {err}")
    return _install(tag)[1]


def main():
    exe = _resolve()
    args = sys.argv[1:]
    if os.name == "nt":
        sys.exit(subprocess.call([str(exe), *args]))
    exe.chmod(exe.stat().st_mode | stat.S_IXUSR | stat.S_IXGRP | stat.S_IXOTH)
    os.execv(str(exe), [str(exe), *args])


if __name__ == "__main__":
    main()
`
