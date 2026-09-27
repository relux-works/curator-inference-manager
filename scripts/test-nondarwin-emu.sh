#!/bin/sh

set -u

usage() {
	cat >&2 <<'USAGE'
Usage:
  scripts/test-nondarwin-emu.sh
  scripts/test-nondarwin-emu.sh --materialize linux|windows SOURCE_ROOT OUTPUT_DIR

The default runs go vet ./... and go test -count=1 ./... against both target
file sets. --materialize creates one verified emulated tree for focused tests
or mutation checks; OUTPUT_DIR must not already exist.
USAGE
}

if ! command -v go >/dev/null 2>&1; then
	echo "go is required" >&2
	exit 2
fi
if ! command -v python3 >/dev/null 2>&1; then
	echo "python3 is required for Go file-set comparison" >&2
	exit 2
fi

script_dir=$(CDPATH='' cd "$(dirname "$0")" && pwd)
repo_root=$(CDPATH='' cd "$script_dir/.." && pwd)
host_goos=$(unset GOOS GOARCH; go env GOOS)
host_goarch=$(unset GOOS GOARCH; go env GOARCH)

export GOPROXY=off
export GOSUMDB=off
export GOTOOLCHAIN=local
export GOFLAGS=

temporary_root=$(mktemp -d "${TMPDIR:-/tmp}/curator-engines-nondarwin-emu.XXXXXX") || exit 2

cleanup() {
	rm -rf "$temporary_root"
}
trap cleanup 0

cat >"$temporary_root/emulate_file_set.py" <<'PYTHON'
import json
import os
import re
import shutil
import sys
from pathlib import Path

GOOS_SUFFIXES = (
    "aix", "android", "darwin", "dragonfly", "freebsd", "illumos",
    "ios", "js", "linux", "netbsd", "openbsd", "plan9", "solaris",
    "wasip1", "windows",
)
BUILD_DIRECTIVE = re.compile(
    r"(?m)^[ \t]*//(?:go:build\b|[ \t]*\+build\b)[^\r\n]*(?:\r?\n|$)"
)


def read_json_stream(path):
    decoder = json.JSONDecoder()
    content = Path(path).read_text(encoding="utf-8")
    values = []
    offset = 0
    while offset < len(content):
        while offset < len(content) and content[offset].isspace():
            offset += 1
        if offset == len(content):
            break
        value, offset = decoder.raw_decode(content, offset)
        values.append(value)
    return values


def emulated_name(name):
    is_test = name.endswith("_test.go")
    suffix = "_test.go" if is_test else ".go"
    stem = name[:-len(suffix)]
    for goos in GOOS_SUFFIXES:
        if stem.endswith("_" + goos):
            stem += "_emu"
            return stem + suffix
    return name


def prepare(source_root, output_root, target_goos, host_goos, target_json, expected_json):
    source_root = Path(source_root).resolve()
    output_root = Path(output_root).resolve()
    if not (source_root / "go.mod").is_file():
        raise SystemExit(f"source root has no go.mod: {source_root}")
    if output_root.exists():
        raise SystemExit(f"output directory already exists: {output_root}")

    target_packages = read_json_stream(target_json)
    selected = {}
    expected = {}
    for package in target_packages:
        import_path = package["ImportPath"]
        if package.get("XTestGoFiles") or package.get("CgoFiles"):
            raise SystemExit(
                f"{import_path}: this emulator compares GoFiles + TestGoFiles; "
                f"XTestGoFiles={package.get('XTestGoFiles', [])} "
                f"CgoFiles={package.get('CgoFiles', [])} must be handled explicitly"
            )
        expected[import_path] = {"GoFiles": [], "TestGoFiles": []}
        package_dir = Path(package["Dir"]).resolve()
        for kind in ("GoFiles", "TestGoFiles"):
            for name in package.get(kind, []):
                relative = (package_dir / name).relative_to(source_root).as_posix()
                if relative in selected:
                    raise SystemExit(f"duplicate Go file in target list: {relative}")
                selected[relative] = (import_path, kind)

    def ignore(directory, names):
        if Path(directory).resolve() == source_root:
            return {name for name in names if name in {".git", ".temp", ".task-board"}}
        return set()

    shutil.copytree(source_root, output_root, ignore=ignore, symlinks=True)
    for directory, directory_names, file_names in os.walk(output_root):
        if Path(directory).resolve() == output_root:
            directory_names[:] = [
                name for name in directory_names
                if name not in {".git", ".temp", ".task-board"}
            ]
        for name in file_names:
            if not name.endswith(".go"):
                continue
            path = Path(directory) / name
            relative = path.relative_to(output_root).as_posix()
            is_selected = relative in selected
            expression = host_goos if is_selected else "!" + host_goos
            text = path.read_text(encoding="utf-8")
            text = BUILD_DIRECTIVE.sub("", text)
            text = f"//go:build {expression}\n\n{text.lstrip()}"

            new_name = emulated_name(name)
            if new_name != name:
                new_path = path.with_name(new_name)
                if new_path.exists():
                    raise SystemExit(f"emulated filename collision: {new_path}")
                path.rename(new_path)
                path = new_path
            path.write_text(text, encoding="utf-8")

            if is_selected:
                import_path, kind = selected[relative]
                expected[import_path][kind].append(path.name)

    for package in expected.values():
        for kind in package:
            package[kind].sort()
    Path(expected_json).write_text(
        json.dumps(expected, sort_keys=True, indent=2) + "\n", encoding="utf-8"
    )

    go_count = sum(len(package["GoFiles"]) for package in expected.values())
    test_count = sum(len(package["TestGoFiles"]) for package in expected.values())
    print(
        f"{target_goos}: prepared {go_count} GoFiles + {test_count} TestGoFiles "
        f"across {len(expected)} packages for host tag {host_goos}"
    )


def compare(expected_json, actual_json):
    expected = json.loads(Path(expected_json).read_text(encoding="utf-8"))
    actual = {}
    for package in read_json_stream(actual_json):
        actual[package["ImportPath"]] = {
            "GoFiles": sorted(package.get("GoFiles", [])),
            "TestGoFiles": sorted(package.get("TestGoFiles", [])),
        }

    mismatches = []
    for import_path in sorted(set(expected) | set(actual)):
        want = expected.get(import_path)
        got = actual.get(import_path)
        if want is None or got is None:
            mismatches.append(
                f"{import_path}: expected package={want is not None}, actual={got is not None}"
            )
            continue
        for kind in ("GoFiles", "TestGoFiles"):
            if want[kind] != got[kind]:
                missing = sorted(set(want[kind]) - set(got[kind]))
                extra = sorted(set(got[kind]) - set(want[kind]))
                mismatches.append(
                    f"{import_path} {kind}: missing={missing} extra={extra}"
                )
    if mismatches:
        print("file-set equality failed:", file=sys.stderr)
        for mismatch in mismatches:
            print(f"  {mismatch}", file=sys.stderr)
        raise SystemExit(1)

    go_count = sum(len(package["GoFiles"]) for package in expected.values())
    test_count = sum(len(package["TestGoFiles"]) for package in expected.values())
    print(
        f"file-set equality: PASS ({go_count} GoFiles + {test_count} TestGoFiles, "
        f"{len(expected)} packages; exit 0)"
    )


if sys.argv[1] == "prepare":
    prepare(*sys.argv[2:])
elif sys.argv[1] == "compare":
    compare(*sys.argv[2:])
else:
    raise SystemExit(f"unknown helper action: {sys.argv[1]}")
PYTHON

prepare_one() {
	platform=$1
	source_root=$2
	output_root=$3
	target_json="$temporary_root/$platform-target.json"
	actual_json="$temporary_root/$platform-emulated.json"
	expected_json="$temporary_root/$platform-expected.json"

	printf '\n== %s: target Go file set ==\n' "$platform"
	printf '+ (cd %s && GOOS=%s GOARCH=%s go list -json ./...)\n' "$source_root" "$platform" "$host_goarch"
	if (cd "$source_root" && GOOS="$platform" GOARCH="$host_goarch" go list -json ./...) >"$target_json"; then
		printf 'PASS [%s] GOOS=%s go list -json ./... (exit 0)\n' "$platform" "$platform"
	else
		status=$?
		printf 'FAIL [%s] go list -json ./... (exit %s)\n' "$platform" "$status" >&2
		return "$status"
	fi

	if python3 "$temporary_root/emulate_file_set.py" prepare "$source_root" "$output_root" "$platform" "$host_goos" "$target_json" "$expected_json"; then
		printf 'PASS [%s] materialize target file set (exit 0)\n' "$platform"
	else
		status=$?
		printf 'FAIL [%s] materialize target Go file set (exit %s)\n' "$platform" "$status" >&2
		return "$status"
	fi

	printf '+ (cd %s && GOOS=%s GOARCH=%s go list -json ./...)\n' "$output_root" "$host_goos" "$host_goarch"
	if (cd "$output_root" && GOOS="$host_goos" GOARCH="$host_goarch" go list -json ./...) >"$actual_json"; then
		printf 'PASS [%s] emulated GOOS=%s go list -json ./... (exit 0)\n' "$platform" "$host_goos"
	else
		status=$?
		printf 'FAIL [%s] emulated go list -json ./... (exit %s)\n' "$platform" "$status" >&2
		return "$status"
	fi

	if python3 "$temporary_root/emulate_file_set.py" compare "$expected_json" "$actual_json"; then
		return 0
	else
		status=$?
		printf 'FAIL [%s] GoFiles + TestGoFiles equality check (exit %s)\n' "$platform" "$status" >&2
		return "$status"
	fi
}

run_validations() {
	platform=$1
	output_root=$2
	status=0

	printf '+ (cd %s && go vet ./...)\n' "$output_root"
	if (cd "$output_root" && GOOS="$host_goos" GOARCH="$host_goarch" go vet ./...); then
		printf 'PASS [%s] go vet ./... (exit 0)\n' "$platform"
	else
		status=$?
		printf 'FAIL [%s] go vet ./... (exit %s)\n' "$platform" "$status" >&2
		failures=1
	fi

	printf '+ (cd %s && go test -count=1 ./...)\n' "$output_root"
	if (cd "$output_root" && GOOS="$host_goos" GOARCH="$host_goarch" go test -count=1 ./...); then
		printf 'PASS [%s] go test -count=1 ./... (exit 0)\n' "$platform"
	else
		status=$?
		printf 'FAIL [%s] go test -count=1 ./... (exit %s)\n' "$platform" "$status" >&2
		failures=1
	fi
}

failures=0
if [ "$#" -gt 0 ]; then
	if [ "$#" -ne 4 ] || [ "$1" != "--materialize" ]; then
		usage
		exit 2
	fi
	platform=$2
	source_root=$3
	output_root=$4
	case "$platform" in
		linux|windows) ;;
		*) usage; exit 2 ;;
	esac
	if [ -e "$output_root" ]; then
		echo "output directory already exists: $output_root" >&2
		exit 2
	fi
	if prepare_one "$platform" "$source_root" "$output_root"; then
		printf 'Materialized %s emulation at %s\n' "$platform" "$output_root"
		exit 0
	else
		status=$?
		exit "$status"
	fi
fi

printf 'Go: '
go version
printf 'Host execution tags: %s/%s; target file sets: linux and windows\n' "$host_goos" "$host_goarch"

for platform in linux windows; do
	output_root="$temporary_root/$platform"
	if prepare_one "$platform" "$repo_root" "$output_root"; then
		run_validations "$platform" "$output_root"
	else
		status=$?
		printf 'FAIL [%s] target file-set preparation (exit %s)\n' "$platform" "$status" >&2
		failures=1
	fi
done

if [ "$failures" -ne 0 ]; then
	echo "Non-Darwin emulation FAILED; see platform-specific failures above." >&2
	exit 1
fi

echo "Non-Darwin emulation PASSED for linux and windows."
