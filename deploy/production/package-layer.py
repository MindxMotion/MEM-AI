"""Package a verified Linux binary for crane mutate; does not build or publish it."""

import pathlib
import sys
import tarfile


def main():
    if len(sys.argv) != 4:
        raise SystemExit("Usage: package-layer.py REPOSITORY LINUX_BINARY OUTPUT_TAR")
    repository, binary, output = map(pathlib.Path, sys.argv[1:])
    with binary.open("rb") as stream:
        header = stream.read(20)
    if header[:6] != b"\x7fELF\x02\x01" or header[18:20] != b"\x3e\x00":
        raise SystemExit("Expected a little-endian Linux amd64 ELF binary")
    members = [(binary, "new-api", 0o755)]
    for name in ("LICENSE", "NOTICE", "THIRD-PARTY-LICENSES.md"):
        members.append((repository / name, f"licenses/{name}", 0o644))
    for source, _, _ in members:
        if source.is_symlink() or not source.is_file():
            raise SystemExit(f"Missing regular build input: {source.name}")
    with output.open("xb") as stream, tarfile.open(fileobj=stream, mode="w") as archive:
        for source, name, mode in members:
            info = archive.gettarinfo(str(source), arcname=name)
            info.uid = info.gid = info.mtime = 0
            info.uname = info.gname = ""
            info.mode = mode
            with source.open("rb") as content:
                archive.addfile(info, content)


if __name__ == "__main__":
    main()
