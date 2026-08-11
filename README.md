# gobackup

An incremental, encrypted, deduplicating backup utility for Amazon S3, inspired by
[tarsnap](https://www.tarsnap.com/).

Backups are chunked with [FastCDC](https://www.usenix.org/conference/atc16/technical-sessions/presentation/xia),
deduplicated globally, compressed, and encrypted on your machine before anything leaves it.
The storage provider only ever sees opaque objects.

```
gobackup init    -repo s3://my-bucket/backups
gobackup backup  ~/Documents ~/Projects
gobackup list
gobackup restore -snapshot latest -target /tmp/restored
gobackup verify
```

## Why it is fast the second time

Two independent mechanisms keep repeat backups cheap:

**Unchanged files are never read.** Each file's size, permissions, modification time,
inode number, and inode change time are compared against the previous snapshot. If they
all match, the file's chunk list is copied straight from the parent snapshot. Backing up a
large, mostly-static directory takes seconds and transfers nothing.

**Changed files only cost what changed.** Files that are new or modified are split into
variable-length chunks whose boundaries are determined by the content, not by fixed
offsets. Inserting bytes in the middle of a file shifts everything after it, but the chunk
boundaries move with the content, so only the chunks around the edit are new. Every chunk
is identified by a keyed hash and looked up in the repository index before it is uploaded.

The effect, as measured by the test suite:

| Scenario | Data | Uploaded |
|---|---|---|
| First backup | 8 MiB | 8 MiB |
| Same data again | 8 MiB | 0 B |
| 1 KiB inserted into a 16 MiB file | 16 MiB | ~1.3 MiB |
| Same 4 MiB file copied to two new paths | 12 MiB | 0 B |

## Installation

```sh
go build -o bin/gobackup ./cmd/gobackup
```

Requires Go 1.25 or later.

## Getting started

### Create a repository

```sh
export GOBACKUP_REPO=s3://my-bucket/backups
gobackup init
```

This generates a new encryption key, protects it with a passphrase you choose, and writes
it to `~/.config/gobackup/gobackup.key`.

> **The keyfile and its passphrase are the only way to read your backups.** Neither can be
> recovered. Store a copy somewhere separate from the repository — losing them means losing
> every backup they protect.

AWS credentials come from the usual places: `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`,
`~/.aws/credentials`, or an instance role.

### Back up

```sh
gobackup backup ~/Documents ~/Projects
gobackup backup -exclude '*.log' -exclude node_modules -tag nightly ~/src
```

The most recent snapshot covering the same paths on the same host is used as the parent
automatically.

Files that cannot be read — permission denied, or a device node or socket — produce a
warning and are skipped, since a handful of unreadable files should not abandon an
otherwise good backup. Failures to *write* to the repository are treated very differently:
they abort the run immediately and no snapshot is written. By the time such an error
surfaces, chunks belonging to files already recorded in the tree may be missing, so
finishing the run would publish a snapshot that cannot be fully restored. A failed backup
leaves unreferenced packs behind, which `verify` reports and a later run simply reuses.

### Inspect

```sh
gobackup list                          # every snapshot
gobackup list -snapshot latest -long   # files inside one snapshot
```

Snapshots can be named by a full ID, any unambiguous prefix, or `latest`.

### Restore

```sh
gobackup restore -snapshot latest -target /tmp/restored
gobackup restore -snapshot a1b2c3d4 -target /tmp/restored -include '*.pdf'
```

Files are written under the target directory reproducing their original absolute paths, so
a snapshot of `/home/you/docs` lands at `/tmp/restored/home/you/docs`. Existing files are
never touched unless you pass `-overwrite`.

### Verify

```sh
gobackup verify              # metadata only: cheap, run it routinely
gobackup verify -read-data   # download and check every byte
```

The default check confirms that every chunk referenced by every snapshot resolves to a pack
that exists and is large enough to hold it. It reads only metadata, so it is cheap enough
to run after every backup, and it catches the failures that actually happen: a missing
pack, an interrupted upload, a snapshot written against an index that never landed.

`-read-data` additionally downloads every pack, decrypts each blob, and recomputes its
keyed ID. Because that ID is an HMAC over the plaintext, a match proves the bytes are
exactly what was backed up. This is the only way to detect silent corruption in storage,
and it costs a full download.

## Configuration

Flags go after the subcommand. Every command accepts:

| Flag | Environment | Meaning |
|---|---|---|
| `-repo` | `GOBACKUP_REPO` | `s3://bucket/prefix`, or a local directory |
| `-keyfile` | `GOBACKUP_KEYFILE` | keyfile path (default `~/.config/gobackup/gobackup.key`) |
| | `GOBACKUP_PASSPHRASE` | passphrase, for unattended runs |
| `-cache-dir` | `GOBACKUP_CACHE_DIR` | local metadata cache directory |
| `-no-cache` | | bypass the local cache |
| `-region` | `AWS_REGION` | AWS region |
| `-endpoint` | | custom S3 endpoint for S3-compatible services |
| `-path-style` | | path-style S3 addressing, needed by most S3-compatible services |
| `-compression` | | `zstd` (default) or `none` |

Setting `GOBACKUP_PASSPHRASE` is what makes scheduled backups possible; otherwise the
passphrase is read from the terminal without echoing.

A local directory works as a repository, which is useful for testing and for backups to an
external or network-mounted disk:

```sh
gobackup init -repo /mnt/backup-disk/repo
```

The layout is identical for both backends, so a repository can be moved between S3 and a
local disk with a plain recursive copy.

## How it works

### Storage layout

```
<prefix>/config                  repository configuration
<prefix>/keys/<id>               optional passphrase-wrapped key copy
<prefix>/packs/<ab>/<id>         ~16 MB containers holding many chunks
<prefix>/indexes/<id>            chunk ID -> pack and byte range
<prefix>/snapshots/<id>          one backup run
```

Everything is encrypted, and every object is immutable — nothing is ever rewritten.

### Packs

S3 charges per request. Storing each ~1 MB chunk as its own object would mean one PUT per
megabyte backed up and one GET per megabyte restored. Chunks are therefore aggregated into
~16 MB packs, cutting request counts by more than an order of magnitude.

Each pack ends with an encrypted header listing its contents, behind a fixed-size footer:

```
[ sealed blob ][ sealed blob ]...[ sealed header ][ header length | magic | version ]
```

Individual blobs are sealed separately, so restoring one chunk costs a single ranged GET of
just that blob. Putting the header at the tail makes packs self-describing: reading the last
few kilobytes of an object recovers the full list of chunks it holds, so a lost index can be
rebuilt from the packs alone.

### Write ordering

A backup writes packs first, then the index, then the snapshot. A snapshot that is visible
in the repository therefore always references data that is already durable. An interrupted
run leaves only unreferenced packs — wasted space, reported by `verify`, but never a
snapshot that cannot be restored.

The repository enforces this rather than trusting callers to. Once any write fails, the
failure is sticky: `Flush` and the snapshot write both refuse to proceed, so a broken
snapshot cannot be published even if the calling code mishandles the original error.

Restores are equally defensive about the target directory. Paths from the repository are
checked for containment lexically, each directory is created without following a symlink in
its place, and every destination directory is re-checked after symlink resolution — so
neither a crafted repository nor a symlink planted in a shared target directory can
redirect writes outside the restore target.

### Restore planning

Rather than fetching chunks one at a time in the order files happen to reference them, a
restore collects every chunk it needs, groups them by pack, sorts them by offset, and merges
chunks that sit within 64 KiB of each other into a single ranged read. One request then
satisfies many chunks.

### Local cache

Index objects and directory trees are cached under `~/.cache/gobackup/<repo-id>/` so repeat
backups do not re-download metadata. The cache is strictly an optimization: entries are
authenticated against the repository key before use, and deleting the directory only costs
time.

## Security

### Cryptography

| Purpose | Construction |
|---|---|
| Keyfile protection | Argon2id (64 MiB, 3 iterations, 4 threads) |
| Object encryption | AES-256-GCM with a per-object HKDF-SHA256 derived key |
| Chunk identifiers | HMAC-SHA256 over the plaintext |

Each object stores a random 32-byte salt; its encryption key is derived as
`HKDF-SHA256(subkey, salt)` and the nonce is fixed at zero. Deriving a fresh key per object
is what makes that safe. The usual hazard with AES-GCM is nonce reuse: with a single
long-lived key and random 96-bit nonces, NIST caps usage at roughly 2^32 messages — a limit
a deduplicating backup tool has no business approaching, since it seals one object per
chunk. Giving every object its own key removes the ceiling entirely while keeping AES-GCM's
hardware acceleration and standard-library implementation. The cost is one HMAC and 48
bytes per object.

Bulk data and metadata use separate subkeys, and every object is bound to its identity
through the AEAD's additional authenticated data, so blobs cannot be swapped between packs
or replayed under a different ID.

Chunk IDs are keyed rather than plain hashes. Deduplication still works — identical content
yields an identical ID for anyone holding the key — but an attacker who can read the
repository cannot test whether a file they already possess is present in your backups. A
plain SHA-256 identifier would leak exactly that.

### What the storage provider can see

Someone with full read access to the bucket learns:

- how many objects exist and how large each one is;
- when objects were written, and hence roughly when you back up;
- the total size of your backups and their approximate growth over time.

They do not learn file contents, file names, directory structure, individual file sizes,
timestamps, which host the data came from, or whether any particular file is present.

### What is out of scope

- **Tampering is detected, not prevented.** Someone with write access can delete or corrupt
  objects. Every such change is caught by `verify` (and by any restore that touches the
  affected data), but the data itself is not recoverable from within the repository.
- **The client is trusted.** Anyone who can read your keyfile and passphrase can read your
  backups.

## Current limitations

Deliberately out of scope for this version:

- **No `prune` or garbage collection.** Snapshots cannot be deleted, and unreferenced packs
  are reported but not removed. Because packs and indexes are immutable and
  content-addressed, adding mark-and-sweep collection later needs no format change.
- **No FUSE mount.**
- **Single writer per repository.** Concurrent backups will not corrupt anything — all
  writes are immutable, uniquely named objects — but they may store some chunks twice.
- **Regular files, directories, and symlinks only.** Device nodes, sockets, and FIFOs are
  skipped. Extended attributes and ACLs are not preserved.

## Development

```sh
make test          # unit and integration tests
make vet
make build
go test -race ./...
```

The S3 backend is held to the same conformance suite as the local backend. To run it
against a real S3-compatible server:

```sh
docker run -d --rm -p 9000:9000 \
  -e MINIO_ROOT_USER=minioadmin -e MINIO_ROOT_PASSWORD=minioadmin \
  minio/minio server /data

GOBACKUP_S3_TEST_ENDPOINT=http://127.0.0.1:9000 \
AWS_ACCESS_KEY_ID=minioadmin AWS_SECRET_ACCESS_KEY=minioadmin AWS_REGION=us-east-1 \
  go test ./internal/backend/s3/
```

### Package layout

| Package | Responsibility |
|---|---|
| `internal/chunker` | FastCDC content-defined chunking |
| `internal/crypto` | keys, authenticated encryption, keyed chunk IDs |
| `internal/backend` | storage interface, plus local and S3 implementations |
| `internal/pack` | pack object format, compression |
| `internal/index` | chunk ID to pack location mapping |
| `internal/repo` | repository configuration and blob storage |
| `internal/snapshot` | tree and snapshot metadata |
| `internal/archiver` | backup pipeline |
| `internal/restorer` | restore pipeline |
| `internal/verify` | integrity checking |
| `internal/cache` | local metadata cache |
| `internal/cli` | command-line interface |

Chunk boundaries are part of the repository format: changing the gear table, the masks, or
the default chunk sizes would silently stop new backups from deduplicating against existing
ones. `TestGoldenBoundaries` exists to catch that.
