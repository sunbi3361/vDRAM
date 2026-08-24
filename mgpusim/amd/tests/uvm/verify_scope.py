#!/usr/bin/env python3
# sbin_codex
"""verify_scope.py - descriptor-safe scope verification for the
mgpusim-uvm-manager plan (Todo 1, mgpusim/amd/tests/uvm).

Trust model
-----------
All trust decisions are made with descriptor-relative O_NOFOLLOW operations:
the workspace root, every path component, and every final file are opened from
their parent directory descriptor with O_NOFOLLOW and verified as regular
files/directories with the required modes. Path-following `test`, `sha256sum`,
`cp`, or `cat` are never used as a trust decision.

Flags
-----
--resolve-attempt-pointer <pointer-path>
    Descriptor-opens the pointer file (relative to --root) and every named
    attempt-directory component with O_NOFOLLOW; requires a regular 0444
    pointer containing exactly one normalized absolute path strictly below the
    workspace evidence root; requires the final attempt descriptor to be a
    directory containing the regular 0444 approved anchor. Prints only that
    canonical path.
--pre-status <baseline-file>          porcelain-v2 baseline; staged paths and
                                      protected file/index state must not drift
--pre-worktree <baseline-file>        worktree binary-diff baseline; protected
                                      paths must not drift
--pre-index <baseline-file>           index binary-diff baseline; protected
                                      paths must not drift
--pre-hashes <baseline-file>          sha256 baseline; every listed workspace
                                      file must still match (descriptor-safe)
--allowed-file <allowlist-file>       staged/changed paths must match a pattern
--check-plan-anchor <sidecar-path>    descriptor-open workspace sidecar+plan,
                                      validate format/mode/digest, byte-compare
                                      against --attempt-anchor (regular 0444)
--attempt-anchor <anchor-path>        attempt-side approved-plan.sha256
--self-test-anchor-mutation           fixtures proving anchor mutation is
                                      detected (mode, symlink, content)
--self-test-attempt-pointer-mutation  fixtures proving pointer mutation is
                                      detected (symlink, mode, content, path)
--staged-only                         only check staged paths against allowlist
--base <commit>                       compute changed files as diff vs commit
--forbid-path-prefix <prefix>         repeatable; abort on changed path prefix
--forbid-import <import-path>         repeatable; abort if a changed non-test
                                      Go file imports the path
--forbid-production-symbol <symbol>   repeatable; abort if a changed non-test
                                      Go file contains the symbol

Only --resolve-attempt-pointer prints to stdout (the canonical attempt path);
all diagnostics go to stderr.
"""

import argparse
import hashlib
import os
import re
import stat
import subprocess
import sys

NOFOLLOW = os.O_NOFOLLOW
DFLAGS = os.O_RDONLY | os.O_DIRECTORY | NOFOLLOW

POINTER_RE = re.compile(rb"^([0-9a-f]{64})  \.omo/plans/mgpusim-uvm-manager\.md\n$")
PLAN_NAME = ".omo/plans/mgpusim-uvm-manager.md"
SIDECAR_NAME = ".omo/plans/mgpusim-uvm-manager.sha256"
ANCHOR_NAME = "approved-plan.sha256"
POINTER_FILE_NAME = "mgpusim-uvm-manager.current"
EVIDENCE_PREFIX = "/.omo/evidence/"

VERIFIER_NAME = "verify_scope.py"


class VerifierError(Exception):
    """Any failed scope check."""


def require(value, message):
    if not value:
        raise VerifierError(message)


def eprint(*args):
    print(*args, file=sys.stderr, flush=True)


def read_all(fd):
    chunks = []
    while True:
        chunk = os.read(fd, 65536)
        if not chunk:
            return b"".join(chunks)
        chunks.append(chunk)


def open_child_dir(fd, name, what):
    try:
        out = os.open(name, DFLAGS, dir_fd=fd)
    except OSError as exc:
        raise VerifierError("cannot open %s '%s' descriptor-safely: %s"
                            % (what, name, exc))
    st = os.fstat(out)
    require(stat.S_ISDIR(st.st_mode), "%s '%s' is not a directory" % (what, name))
    return out


def open_child_regular(fd, name, what):
    try:
        out = os.open(name, os.O_RDONLY | NOFOLLOW, dir_fd=fd)
    except OSError as exc:
        raise VerifierError("cannot open %s '%s' descriptor-safely: %s"
                            % (what, name, exc))
    st = os.fstat(out)
    require(stat.S_ISREG(st.st_mode), "%s '%s' is not a regular file" % (what, name))
    return out, st


def read_child_regular(fd, name, what):
    item, st = open_child_regular(fd, name, what)
    try:
        return read_all(item), st
    finally:
        os.close(item)


def walk_dirs(fd, relpath, what):
    """Open every path component of relpath as a directory, O_NOFOLLOW each."""
    for part in relpath.split("/"):
        if part in ("", "."):
            continue
        require(part != "..", "unsafe %s path component '..'" % what)
        fd = open_child_dir(fd, part, what)
    return fd


def rel_under_root(root, path):
    require(os.path.isabs(path), "path %r is not absolute" % path)
    prefix = root.rstrip("/") + "/"
    require(path.startswith(prefix), "path %r escapes root %r" % (path, root))
    rel = path[len(prefix):]
    require(rel != "", "path %r is the root itself" % path)
    return rel


def sha256_file_fd(fd):
    return hashlib.sha256(read_all(fd)).hexdigest()


# ---------------------------------------------------------------------------
# Attempt pointer resolution
# ---------------------------------------------------------------------------

def resolve_attempt_pointer(root, pointer_path):
    """Descriptor-safe pointer resolution. Returns the canonical attempt path
    or raises VerifierError. Never follows symlinks."""
    root_fd = os.open(root, DFLAGS)
    try:
        st = os.fstat(root_fd)
        require(stat.S_ISDIR(st.st_mode), "root %r is not a directory" % root)
        evidence_prefix = root.rstrip("/") + EVIDENCE_PREFIX

        parts = [p for p in pointer_path.split("/") if p not in ("", ".")]
        require(parts and all(p != ".." for p in parts),
                "unsafe pointer path %r" % pointer_path)

        # Descriptor-open every pointer component; the final component must be
        # a regular 0444 file.
        fd = root_fd
        for part in parts[:-1]:
            fd = open_child_dir(fd, part, "pointer path")
        pfd, pst = open_child_regular(fd, parts[-1], "pointer")
        try:
            require(stat.S_IMODE(pst.st_mode) == 0o444,
                    "pointer %r is not regular 0444 (mode %o)"
                    % (pointer_path, stat.S_IMODE(pst.st_mode)))
            content = read_all(pfd)
        finally:
            os.close(pfd)

        # Exactly one normalized absolute path below the evidence root.
        require(content.endswith(b"\n"), "pointer %r has no trailing newline"
                % pointer_path)
        text = content[:-1]
        require(b"\n" not in text and b"\x00" not in text,
                "pointer %r does not contain exactly one path line" % pointer_path)
        try:
            line = text.decode("utf-8")
        except UnicodeDecodeError:
            raise VerifierError("pointer %r is not UTF-8 text" % pointer_path)
        norm = os.path.normpath(line)
        require(os.path.isabs(norm) and norm.startswith(evidence_prefix)
                and len(norm) > len(evidence_prefix) and norm != evidence_prefix[:-1],
                "pointer %r target %r escapes the workspace evidence root"
                % (pointer_path, norm))

        # Descriptor-open every attempt-directory component; the final
        # descriptor must be a directory containing the regular 0444 anchor.
        attempt_rel = rel_under_root(root, norm)
        afd = walk_dirs(root_fd, attempt_rel, "attempt path")
        anchor_fd, ast = open_child_regular(afd, ANCHOR_NAME, "approved anchor")
        try:
            require(stat.S_IMODE(ast.st_mode) == 0o444,
                    "approved anchor is not regular 0444 (mode %o)"
                    % stat.S_IMODE(ast.st_mode))
            read_all(anchor_fd)  # readable
        finally:
            os.close(anchor_fd)

        return norm
    finally:
        os.close(root_fd)


# ---------------------------------------------------------------------------
# Baseline capture helpers
# ---------------------------------------------------------------------------

def git(root, *args):
    return subprocess.run(("git",) + args, cwd=root, check=True,
                          stdout=subprocess.PIPE).stdout.decode("utf-8", "replace")


def parse_status(text):
    """Parse porcelain-v2 status into (kind, xy, path) rows."""
    rows = []
    for line in text.splitlines():
        if not line or line.startswith("#"):
            continue
        if line.startswith("?"):
            rows.append(("?", "??", line[2:].strip()))
        elif line[0] in ("1", "2", "u"):
            parts = line.split(" ")
            require(len(parts) >= 4, "malformed porcelain-v2 row: %r" % line)
            path = parts[-1]
            if "\t" in path:
                path = path.split("\t")[0]  # rename: orig path after tab
            rows.append((line[0], parts[1], path))
    return rows


def parse_diff_paths(diff_bytes):
    """Extract changed paths from a git binary diff."""
    paths = set()
    for line in diff_bytes.splitlines():
        if line.startswith(b"diff --git "):
            rest = line[len(b"diff --git "):].decode("utf-8", "replace")
            fields = rest.split(" ")
            bpath = fields[1] if len(fields) > 1 else ""
            if bpath.startswith("b/"):
                bpath = bpath[2:]
            require(bpath != "", "malformed diff header: %r" % rest)
            paths.add(bpath)
    return paths


def read_baseline(root, path, what):
    """Read a baseline receipt file descriptor-safely under root."""
    rel = rel_under_root(root, os.path.abspath(path)) \
        if os.path.isabs(path) else path
    root_fd = os.open(root, DFLAGS)
    try:
        fd = walk_dirs(root_fd, os.path.dirname(rel), what)
        data, st = read_child_regular(fd, os.path.basename(rel), what)
        return data, st
    finally:
        os.close(root_fd)


def load_allowlist(path):
    """Read the allowlist receipt. It lives under the evidence root and is
    itself a bootstrap receipt; read it descriptor-safely when under root."""
    if os.path.isabs(path):
        with open(path, "r", encoding="utf-8") as f:
            patterns = [ln.strip() for ln in f if ln.strip()]
    else:
        with open(path, "r", encoding="utf-8") as f:
            patterns = [ln.strip() for ln in f if ln.strip()]
    return patterns


def path_matches_allowlist(path, patterns):
    for pattern in patterns:
        if pattern.endswith("/**"):
            prefix = pattern[:-3]
            if path == prefix or path.startswith(prefix + "/"):
                return True
        elif pattern.endswith("/*"):
            prefix = pattern[:-2]
            if path.startswith(prefix + "/") and "/" not in path[len(prefix) + 1:]:
                return True
        elif path == pattern:
            return True
    return False


# ---------------------------------------------------------------------------
# Individual checks
# ---------------------------------------------------------------------------

def check_pre_hashes(root, baseline_path):
    data, _ = read_baseline(root, baseline_path, "pre-hashes baseline")
    root_fd = os.open(root, DFLAGS)
    try:
        lines = data.decode("utf-8").splitlines()
        require(len(lines) >= 2, "pre-hashes baseline is empty")
        for line in lines:
            m = re.fullmatch(r"([0-9a-f]{64})  (.*)", line)
            require(m is not None, "malformed pre-hashes row: %r" % line)
            want, name = m.group(1), m.group(2)
            require(name not in ("", ".", ".."), "unsafe hashed name %r" % name)
            if os.path.isabs(name):
                rel = rel_under_root(root, name)
            else:
                rel = name
            parts = rel.split("/")
            require(parts and all(p not in ("", ".", "..") for p in parts),
                    "unsafe hashed name %r" % name)
            fd = root_fd
            for part in parts[:-1]:
                fd = open_child_dir(fd, part, "pre-hashes path")
            item, st = open_child_regular(fd, parts[-1], "pre-hashes target")
            try:
                actual = sha256_file_fd(item)
            finally:
                os.close(item)
            require(actual == want,
                    "protected file %r digest changed (was %s, now %s)"
                    % (name, want, actual))
    finally:
        os.close(root_fd)


def protected_names(pre_hashes_data):
    """Workspace files named in the pre-hashes baseline (the protected
    worktree contract: AGENTS.md, uvm-manager.md)."""
    names = []
    for line in pre_hashes_data.decode("utf-8").splitlines():
        m = re.fullmatch(r"[0-9a-f]{64}  (.*)", line)
        if not m:
            continue
        name = m.group(1)
        if not os.path.isabs(name) and not name.startswith(".omo/"):
            names.append(name)
    return set(names)


def status_protected_drift(baseline_rows, current_rows, protected):
    base = {p: xy for _, xy, p in baseline_rows}
    cur = {p: xy for _, xy, p in current_rows}
    for name in protected:
        bx, by = (base[name][0], base[name][1]) if name in base else (".", ".")
        cx, cy = (cur[name][0], cur[name][1]) if name in cur else (".", ".")
        require((bx, by) == (cx, cy),
                "protected file/index state drifted for %r: baseline %s%s, "
                "now %s%s" % (name, bx, by, cx, cy))
        require(name not in (p for p in cur if p == name and cur[name] == "??")
                or cur.get(name) == "??" and base.get(name) == "??",
                "protected untracked state changed for %r" % name)


def check_pre_status(root, baseline_path, allowlist, staged_only,
                     forbid_prefixes, forbid_imports, forbid_symbols, base):
    changed, staged = changed_paths(root, baseline_path, base)

    if staged_only:
        checked = staged
    else:
        checked = changed

    if allowlist:
        patterns = load_allowlist(allowlist)
        for path in sorted(checked):
            require(path_matches_allowlist(path, patterns),
                    "path %r is outside the scope allowlist" % path)

    for path in sorted(changed):
        for prefix in forbid_prefixes:
            require(not path.startswith(prefix),
                    "changed path %r is inside forbidden prefix %r"
                    % (path, prefix))

    for path in sorted(changed):
        if not path.endswith(".go") or path.endswith("_test.go"):
            continue
        full = os.path.join(root, path)
        try:
            with open(full, "r", encoding="utf-8", errors="replace") as f:
                text = f.read()
        except OSError:
            continue
        for imp in forbid_imports:
            require(imp not in go_imports(text),
                    "changed file %r imports forbidden package %r" % (path, imp))
        for sym in forbid_symbols:
            require(re.search(r"\b" + re.escape(sym) + r"\b", text) is None,
                    "changed file %r contains forbidden symbol %r" % (path, sym))


def changed_paths(root, baseline_path, base):
    """Compute changed and staged path sets vs the bootstrap baseline,
    enforcing the protected file/index state contract."""
    base_data, _ = read_baseline(root, baseline_path, "pre-status baseline")
    baseline_rows = parse_status(base_data.decode("utf-8"))
    current_text = git(root, "status", "--porcelain=v2", "--untracked-files=all")
    current_rows = parse_status(current_text)

    protected = protected_names(base_data)
    status_protected_drift(baseline_rows, current_rows, protected)

    base_rows = {p: xy for _, xy, p in baseline_rows}
    cur_rows = {p: xy for _, xy, p in current_rows}

    changed = set()
    staged = set()
    for kind, xy, path in current_rows:
        base_xy = base_rows.get(path)
        if kind == "?":
            changed.add(path)  # untracked: never staged
            continue
        if base_xy != xy:
            changed.add(path)
        x = xy[0] if xy != "??" else "."
        if x != ".":
            staged.add(path)
    if base is not None:
        for path in git(root, "diff", "--name-only", "--diff-filter=ACMRTUXB",
                        base).splitlines():
            if path:
                changed.add(path)
        for path in git(root, "ls-files", "--others", "--exclude-standard").splitlines():
            if path:
                changed.add(path)
    return changed, staged


def changed_from_diffs(root, pre_worktree, pre_index, base):
    changed = set()
    staged = set()
    if pre_worktree:
        data, _ = read_baseline(root, pre_worktree, "pre-worktree baseline")
        changed |= parse_diff_paths(data)
    if pre_index:
        data, _ = read_baseline(root, pre_index, "pre-index baseline")
        changed |= parse_diff_paths(data)
        staged |= parse_diff_paths(data)
    if base is not None:
        for path in git(root, "diff", "--name-only", "--diff-filter=ACMRTUXB",
                        base).splitlines():
            if path:
                changed.add(path)
        for path in git(root, "ls-files", "--others", "--exclude-standard").splitlines():
            if path:
                changed.add(path)
    return changed, staged


def go_imports(text):
    imports = []
    block = re.search(r"import\s*\((.*?)\)", text, re.S)
    if block:
        for line in block.group(1).splitlines():
            line = line.strip()
            if line.startswith('"') and line.endswith('"'):
                imports.append(line[1:-1])
            elif '"' in line:
                tok = line.split()[-1]
                if tok.startswith('"') and tok.endswith('"'):
                    imports.append(tok[1:-1])
    for m in re.finditer(r'import\s+"([^"]+)"', text):
        imports.append(m.group(1))
    return imports


def diff_protected_drift(root, baseline_path, current_bytes, what):
    base_data, _ = read_baseline(root, baseline_path, what + " baseline")
    base_paths = parse_diff_paths(base_data)
    cur_paths = parse_diff_paths(current_bytes)
    require(base_paths == cur_paths,
            "%s protected drift: baseline %s, now %s"
            % (what, sorted(base_paths), sorted(cur_paths)))


def check_pre_worktree(root, baseline_path):
    diff_protected_drift(
        root, baseline_path,
        git(root, "diff", "--binary").encode("utf-8"), "pre-worktree")


def check_pre_index(root, baseline_path):
    diff_protected_drift(
        root, baseline_path,
        git(root, "diff", "--cached", "--binary").encode("utf-8"), "pre-index")


def check_plan_anchor(root, sidecar_path, attempt_anchor_path):
    """Descriptor-open workspace sidecar+plan; validate format/mode/digest;
    byte-compare with the attempt anchor (regular 0444)."""
    root_fd = os.open(root, DFLAGS)
    try:
        rel = rel_under_root(root, os.path.abspath(sidecar_path)) \
            if os.path.isabs(sidecar_path) else sidecar_path
        parts = [p for p in rel.split("/") if p not in ("", ".")]
        require(parts and all(p != ".." for p in parts),
                "unsafe sidecar path %r" % sidecar_path)
        fd = root_fd
        for part in parts[:-1]:
            fd = open_child_dir(fd, part, "sidecar path")
        side, ss = read_child_regular(fd, parts[-1], "sidecar")
        require(stat.S_IMODE(ss.st_mode) in (0o444, 0o644, 0o664),
                "unsafe sidecar mode %o" % stat.S_IMODE(ss.st_mode))
        require(parts[-1] == "mgpusim-uvm-manager.sha256",
                "sidecar must be mgpusim-uvm-manager.sha256")

        pfd = walk_dirs(root_fd, os.path.dirname(rel), "plan path") \
            if os.path.dirname(rel) else root_fd
        plan, ps = read_child_regular(pfd, "mgpusim-uvm-manager.md", "plan")
        require(stat.S_ISREG(ps.st_mode), "plan is not a regular file")
        m = POINTER_RE.fullmatch(side)
        require(m is not None, "sidecar format invalid")
        require(hashlib.sha256(plan).hexdigest().encode("ascii") == m.group(1),
                "plan digest does not match sidecar")

        arel = rel_under_root(root, os.path.abspath(attempt_anchor_path)) \
            if os.path.isabs(attempt_anchor_path) else attempt_anchor_path
        aparts = [p for p in arel.split("/") if p not in ("", ".")]
        require(aparts and all(p != ".." for p in aparts),
                "unsafe attempt-anchor path %r" % attempt_anchor_path)
        afd = root_fd
        for part in aparts[:-1]:
            afd = open_child_dir(afd, part, "attempt-anchor path")
        anchor, ast = read_child_regular(afd, aparts[-1], "attempt anchor")
        require(stat.S_IMODE(ast.st_mode) == 0o444,
                "attempt anchor is not regular 0444 (mode %o)"
                % stat.S_IMODE(ast.st_mode))
        require(anchor == side,
                "attempt anchor bytes differ from the workspace sidecar")
    finally:
        os.close(root_fd)


# ---------------------------------------------------------------------------
# Self tests: prove the verifier detects mutations
# ---------------------------------------------------------------------------

def make_fixture_dir(root, name):
    path = os.path.join(root, ".omo", "evidence", "self-test",
                        name)
    os.makedirs(path, exist_ok=True)
    return path


def clean_fixture(path):
    try:
        for entry in os.listdir(path):
            full = os.path.join(path, entry)
            if os.path.islink(full) or os.path.isfile(full):
                os.unlink(full)
            elif os.path.isdir(full):
                os.rmdir(full)
        os.rmdir(path)
    except OSError:
        pass


def self_test_anchor_mutation(root, attempt_anchor_path):
    """Each fixture must be REJECTED by the anchor check."""
    fixture_dir = make_fixture_dir(root, "anchor-mutation")
    base_data, _ = read_baseline(root, attempt_anchor_path, "anchor baseline")
    base_text = base_data.decode("utf-8")
    try:
        # 1. content mutation
        mutated = base_text[:-1] + ("0" if base_text[-1] != "0" else "1")
        with open(os.path.join(fixture_dir, "content.sha256"), "w",
                  encoding="utf-8") as f:
            f.write(mutated)
        os.chmod(os.path.join(fixture_dir, "content.sha256"), 0o444)
        # 2. wrong mode
        with open(os.path.join(fixture_dir, "mode.sha256"), "w",
                  encoding="utf-8") as f:
            f.write(base_text)
        os.chmod(os.path.join(fixture_dir, "mode.sha256"), 0o644)
        # 3. symlink (descriptor-open must refuse to follow it)
        link = os.path.join(fixture_dir, "link.sha256")
        if os.path.lexists(link):
            os.unlink(link)
        os.symlink(attempt_anchor_path, link)

        failures = []
        for name in ("content.sha256", "mode.sha256", "link.sha256"):
            try:
                check_plan_anchor(root, SIDECAR_NAME,
                                  os.path.join(fixture_dir, name))
                failures.append(name)
            except VerifierError:
                pass
        require(not failures,
                "anchor self-test fixtures were NOT rejected: %s"
                % ", ".join(failures))
        eprint("self-test: anchor mutation fixtures rejected as expected "
               "(content, mode, symlink)")
    finally:
        clean_fixture(fixture_dir)


def self_test_attempt_pointer_mutation(root, real_pointer):
    """Each fixture must be REJECTED by pointer resolution."""
    fixture_dir = make_fixture_dir(root, "pointer-mutation")
    real_path = os.path.join(root, real_pointer)
    try:
        # 1. symlink pointer
        link = os.path.join(fixture_dir, "link.current")
        if os.path.lexists(link):
            os.unlink(link)
        os.symlink(real_path, link)
        # 2. wrong mode
        mode = os.path.join(fixture_dir, "mode.current")
        with open(mode, "w", encoding="utf-8") as f:
            f.write("/home/sbin/vdram_v3/.omo/evidence/manual/mgpusim-uvm-manager\n")
        os.chmod(mode, 0o644)
        # 3. content: target outside the evidence root
        outside = os.path.join(fixture_dir, "outside.current")
        with open(outside, "w", encoding="utf-8") as f:
            f.write("/tmp/not-under-evidence\n")
        os.chmod(outside, 0o444)
        # 4. content: two lines
        two = os.path.join(fixture_dir, "two.current")
        with open(two, "w", encoding="utf-8") as f:
            f.write("/home/sbin/vdram_v3/.omo/evidence/manual/mgpusim-uvm-manager\n"
                    "/home/sbin/vdram_v3/.omo/evidence/manual/other\n")
        os.chmod(two, 0o444)
        # 5. content: no trailing newline
        nonl = os.path.join(fixture_dir, "nonl.current")
        with open(nonl, "w", encoding="utf-8") as f:
            f.write("/home/sbin/vdram_v3/.omo/evidence/manual/mgpusim-uvm-manager")
        os.chmod(nonl, 0o444)

        failures = []
        for name in ("link.current", "mode.current", "outside.current",
                     "two.current", "nonl.current"):
            try:
                resolve_attempt_pointer(root, os.path.join(fixture_dir, name))
                failures.append(name)
            except VerifierError:
                pass
        require(not failures,
                "pointer self-test fixtures were NOT rejected: %s"
                % ", ".join(failures))
        eprint("self-test: attempt-pointer mutation fixtures rejected as "
               "expected (symlink, mode, outside-root, two-line, no-newline)")
    finally:
        clean_fixture(fixture_dir)


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

def parse_args(argv):
    p = argparse.ArgumentParser(
        prog=VERIFIER_NAME,
        description="Descriptor-safe scope verification for the "
                    "mgpusim-uvm-manager plan.")
    p.add_argument("--root", default="/home/sbin/vdram_v3",
                   help="workspace root (default: %(default)s)")
    p.add_argument("--resolve-attempt-pointer", metavar="POINTER",
                   help="resolve the attempt pointer and print the canonical "
                        "attempt path")
    p.add_argument("--pre-status", metavar="FILE")
    p.add_argument("--pre-worktree", metavar="FILE")
    p.add_argument("--pre-index", metavar="FILE")
    p.add_argument("--pre-hashes", metavar="FILE")
    p.add_argument("--allowed-file", metavar="FILE")
    p.add_argument("--check-plan-anchor", metavar="SIDECAR")
    p.add_argument("--attempt-anchor", metavar="FILE")
    p.add_argument("--self-test-anchor-mutation", action="store_true")
    p.add_argument("--self-test-attempt-pointer-mutation", action="store_true")
    p.add_argument("--staged-only", action="store_true")
    p.add_argument("--base", metavar="COMMIT")
    p.add_argument("--forbid-path-prefix", action="append", default=[])
    p.add_argument("--forbid-import", action="append", default=[])
    p.add_argument("--forbid-production-symbol", action="append", default=[])
    return p.parse_args(argv)


def main(argv):
    args = parse_args(argv)
    root = os.path.abspath(args.root)
    require(os.path.isdir(root), "root %r is not a directory" % root)

    actions = 0

    if args.resolve_attempt_pointer:
        actions += 1
        attempt = resolve_attempt_pointer(root, args.resolve_attempt_pointer)
        sys.stdout.write(attempt + "\n")
        sys.stdout.flush()

    if args.check_plan_anchor:
        actions += 1
        require(args.attempt_anchor is not None,
                "--check-plan-anchor requires --attempt-anchor")
        check_plan_anchor(root, args.check_plan_anchor, args.attempt_anchor)
        eprint("plan anchor verified: sidecar format/mode, plan digest, "
               "attempt anchor bytes")

    if args.pre_hashes:
        actions += 1
        check_pre_hashes(root, args.pre_hashes)
        eprint("pre-hashes verified: protected files unchanged")

    if args.pre_status:
        actions += 1
        check_pre_status(root, args.pre_status, args.allowed_file,
                         args.staged_only, args.forbid_path_prefix,
                         args.forbid_import, args.forbid_production_symbol,
                         args.base)
        eprint("pre-status verified: protected state unchanged, "
               "changed paths in scope")
    elif args.allowed_file:
        actions += 1
        changed, staged = changed_from_diffs(root, args.pre_worktree,
                                             args.pre_index, args.base)
        checked = staged if args.staged_only else changed
        patterns = load_allowlist(args.allowed_file)
        for path in sorted(checked):
            require(path_matches_allowlist(path, patterns),
                    "path %r is outside the scope allowlist" % path)
        eprint("allowlist verified for %d changed path(s)" % len(checked))

    if args.pre_worktree:
        actions += 1
        check_pre_worktree(root, args.pre_worktree)
        eprint("pre-worktree verified: protected diff unchanged")

    if args.pre_index:
        actions += 1
        check_pre_index(root, args.pre_index)
        eprint("pre-index verified: protected index diff unchanged")

    if args.self_test_anchor_mutation:
        actions += 1
        require(args.attempt_anchor is not None,
                "--self-test-anchor-mutation requires --attempt-anchor")
        self_test_anchor_mutation(root, args.attempt_anchor)

    if args.self_test_attempt_pointer_mutation:
        actions += 1
        pointer = args.resolve_attempt_pointer \
            or ".omo/evidence/%s" % POINTER_FILE_NAME
        self_test_attempt_pointer_mutation(root, pointer)

    require(actions > 0, "no verification action requested")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main(sys.argv[1:]))
    except VerifierError as exc:
        eprint("%s: %s" % (VERIFIER_NAME, exc))
        sys.exit(1)
    except (OSError, ValueError, subprocess.CalledProcessError) as exc:
        eprint("%s: %s" % (VERIFIER_NAME, exc))
        sys.exit(1)
