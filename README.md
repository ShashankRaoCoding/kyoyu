# kyoyu

Turn a prebuilt binary into a pip-installable package and upload it to PyPI, with no Python
build tooling needed to make the wheel.

```bash
kyoyu -bin ./mycli
```

kyoyu is a single Go program. Given a compiled binary it:

1. Infers the package and command name from the binary's file name.
2. Generates a small Python project around it (`pyproject.toml`, `setup.py`, a launcher
   shim and a README) in `./<name>-pypi`.
3. Builds a platform-tagged wheel (for example `manylinux_2_17_x86_64`) by reading the binary's
   own header, so a cross-compiled binary is tagged for its target, not for your machine.
4. Checks the wheel with `twine check`, looks the package up on the index, asks you to
   confirm, then uploads it with `twine`.

Users then run `pip install mycli` and type `mycli`. The launcher marks the bundled binary
executable and runs it. kyoyu does not touch `PATH` or any shell config; that is up to the
program itself.

## Install

```bash
go install github.com/ShashankRaoCoding/kyoyu@latest
```

Uploading needs [twine](https://twine.readthedocs.io/) (`pip install twine`) and a PyPI token
configured the usual way (`~/.pypirc` or `TWINE_*` environment variables). Building the wheel
does not.

## Usage

```
kyoyu -bin <binary> [flags]
```

| Flag | Meaning |
| --- | --- |
| `-bin` | Path to the compiled binary (required). |
| `-name` | Package name on PyPI. Default: the binary's file name. |
| `-version` | Release version. Default: `0.1.0`, or the existing project's version. |
| `-project` | Project directory to create. Default: `./<name>-pypi`. |
| `-plat` | Wheel platform tag. Default: detected from the binary. |
| `-description` | One-line package description. |
| `-repository` | A `.pypirc` repository name such as `testpypi`, or an upload URL. |
| `-no-upload` | Build the wheel but do not upload it. |
| `-y` | Skip the upload confirmation prompt. |
| `-force` | Overwrite existing generated project files. |
| `-verbose` | Print each step and run `twine` with `--verbose`. |

Try a release on TestPyPI first:

```bash
kyoyu -bin ./mycli -version 0.2.0 -repository testpypi
```

## One package, many platforms

A wheel holds one binary, so a multi-platform tool is several wheels under the same version.
Run kyoyu once per build with the same `-name` and `-version`; each run adds that platform's
wheel to the release.

```bash
GOOS=linux  GOARCH=amd64 go build -o dist/mycli && kyoyu -bin dist/mycli -name mycli -version 1.0.0 -y
GOOS=darwin GOARCH=arm64 go build -o dist/mycli && kyoyu -bin dist/mycli -name mycli -version 1.0.0 -y
```

Detected tags: Linux (glibc `manylinux_2_17`, musl `musllinux_1_2`, on x86_64, aarch64, i686,
armv7l), macOS (x86_64, arm64, universal2) and Windows (`win_amd64`, `win_arm64`, `win32`).

Re-running with an existing project keeps your edited files, replaces the bundled binary, and
applies `-version` if you pass one. PyPI never accepts the same file twice, so a new upload needs
a new version.

## Installer mode (optional, experimental)

Instead of bundling the binary, kyoyu can publish one small pure-Python wheel (`py3-none-any`)
whose launcher downloads the binary from your GitHub Releases the first time it runs. The PyPI
package then never changes when your program does: a new release is just a new GitHub Release.

```bash
kyoyu -github ShashankRaoCoding/yozora -name yozora -command yzr
```

| Flag | Meaning |
| --- | --- |
| `-github` | `owner/repo` whose Releases hold the binaries. Turns installer mode on. |
| `-command` | The command users type. Default: `-name`. |
| `-asset` | Release asset name. Default `{script}_{os}_{arch}{ext}`. |
| `-checksums` | Release file listing SHA-256 sums. Default `SHA256SUMS`. |

In `-asset`, `os` is `linux`, `darwin` or `windows`; `arch` is `amd64`, `arm64`, `386` or `arm`;
`ext` is `.exe` on Windows. With the defaults a release for the command `yzr` contains
`yzr_linux_amd64`, `yzr_darwin_arm64`, `yzr_windows_amd64.exe` and so on, plus a `SHA256SUMS`
file in `sha256sum` format.

How the launcher behaves:

- **Verification:** it downloads the asset for the user's platform, checks it against the
  release's checksums file, and refuses to run it on a mismatch or if the file is missing. This
  guards against corrupted downloads; it does not protect against a compromised release.
- **Versions:** it runs the newest release by default, looking for a newer one at most once a day
  and falling back to the cached copy when offline. Setting `<PACKAGE>_VERSION` (for example
  `YOZORA_VERSION=1.2.3`) runs that release instead; `1.2.3` and `v1.2.3` both work.
- **Cache:** binaries are kept in the user's cache directory (`~/.cache` or `$XDG_CACHE_HOME` on
  Linux, `~/Library/Caches` on macOS, `%LOCALAPPDATA%` on Windows). The first run needs network
  access; on a cluster, run the command once on a login node so compute nodes can use the cache.

Installer mode has only been tested against a local test server, not against real GitHub
Releases. The wheel is checked with `twine check`, but publishing it is up to you.

## Before you publish

- Make sure you have the right to distribute the binary and that it is what the package
  description says it is.
- PyPI's terms and [Acceptable Use Policy](https://policies.python.org/pypi.org/Acceptable-Use-Policy/)
  apply to everything you upload. Prebuilt binaries are widely published, but PyPI has no formal
  policy on non-Python programs, so check the current terms for your own case.
- Uploads cannot be undone or overwritten. Use `-repository testpypi` and read the prompt.

## Licence

GNU General Public License v3.0. See [LICENSE](LICENSE).
