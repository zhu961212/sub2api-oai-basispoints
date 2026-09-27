"""Export current source files for GitHub; never copy Git history or build output."""
import argparse
import hashlib
import json
from pathlib import Path, PurePosixPath
import subprocess
import zipfile

ROOT_FILES = {
    ".gitattributes", ".gitignore", "README.md", "THIRD_PARTY_NOTICES.md",
    "LICENSE", "LICENSE.md", "go.mod", "go.sum", "manifest.source.json",
    "build.ps1", "build.sh",
}
SOURCE_DIRS = {"cmd", "internal", "third_party", "tools", "ui", "docs", ".github"}
SOURCE_SUFFIXES = {
    ".go", ".mod", ".sum", ".md", ".json", ".proto", ".css", ".js", ".html",
    ".cjs", ".mjs", ".py", ".ps1", ".sh", ".yml", ".yaml",
}
SOURCE_FIXTURES = {
    "tools/host-diagnostic-integration/diagnostic_jobs_host_test.go.txt",
    "tools/host-relay-integration/relay_scope_host_test.go.txt",
    "tools/host-relay-integration/signed_package_host_test.go.txt",
}
EXCLUDED_PARTS = {
    ".git", "build", "dist", "node_modules", "__pycache__", ".codex",
    ".claude", ".workbuddy", ".idea", ".vscode",
}

def source_path_allowed(name):
    path = PurePosixPath(name)
    if path.is_absolute() or ".." in path.parts or not path.parts:
        return False
    if any(part.lower() in EXCLUDED_PARTS for part in path.parts):
        return False
    lower = path.name.lower()
    if lower.startswith((".env", "credentials", "secrets", "accounts", "sub2api-account-")):
        return False
    if lower == "auth.json" or lower.endswith(".local.json"):
        return False
    if len(path.parts) == 1:
        return path.name in ROOT_FILES
    if path.suffix.lower() == ".patch":
        return path.parts[:2] == ("docs", "host-patches")
    if path.as_posix() in SOURCE_FIXTURES:
        return True
    return path.parts[0] in SOURCE_DIRS and (
        path.suffix.lower() in SOURCE_SUFFIXES or path.name == "LICENSE"
    )

def export_source(root, output=None):
    root = Path(root).resolve()
    manifest = json.loads((root / "manifest.source.json").read_text(encoding="utf-8"))
    version = manifest["version"]
    prefix = "sub2api-oai-basispoints-" + version
    # Include current edits and new files without requiring an initial commit.
    listing = subprocess.run(
        ["git", "ls-files", "--cached", "--others", "--exclude-standard", "-z"],
        cwd=root, check=True, capture_output=True,
    ).stdout.decode("utf-8").split("\0")
    paths = []
    for name in sorted(set(filter(None, listing))):
        if not source_path_allowed(name):
            continue
        path = root / name
        if not path.is_file():
            continue
        if path.is_symlink() or not path.resolve().is_relative_to(root):
            raise ValueError("Source entry resolves outside the project or is a symlink")
        paths.append(name)
    if not {"README.md", "go.mod", "manifest.source.json"}.issubset(paths):
        raise ValueError("Required project source files are missing")
    output = Path(output) if output else root / "dist" / (prefix + "-github-source.zip")
    output.parent.mkdir(parents=True, exist_ok=True)
    with zipfile.ZipFile(output, "w", compression=zipfile.ZIP_DEFLATED, compresslevel=9) as bundle:
        for name in paths:
            bundle.write(root / name, prefix + "/" + name)
    with zipfile.ZipFile(output) as bundle:
        if bundle.testzip() is not None:
            raise ValueError("Source archive verification failed")
    digest = hashlib.sha256(output.read_bytes()).hexdigest()
    output.with_name(output.name + ".sha256").write_text(
        digest + "  " + output.name + "\n", encoding="ascii"
    )
    return {"path": str(output.resolve()), "files": len(paths), "sha256": digest}

if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()
    print(json.dumps(export_source(Path(__file__).resolve().parents[1], args.output), ensure_ascii=False))
