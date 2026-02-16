# Artifacts

[fundamentals.md](fundamentals.md) establishes Bureau's primitives;
[architecture.md](architecture.md) defines the runtime topology of
launcher, daemon, and service processes;
[information-architecture.md](information-architecture.md) defines the
Matrix data model. This document describes Bureau's content-addressable
storage layer — the system that stores, deduplicates, caches, transfers,
and encrypts the data artifacts that agents produce and consume.

---

## Why Content-Addressable Storage

Agents produce data at scale. A single coding agent generates
conversation logs, screenshots, compiled binaries, test outputs, and
coverage reports. A team of agents working on a machine learning
pipeline produces model weights, dataset snapshots, evaluation results,
and training checkpoints. A fleet of machines running Bureau accumulates
this across hundreds of principals over weeks of operation.

Bureau needs three properties for this data:

- **Deduplication.** The same file stored by two agents — or two
  versions of a file that differ by a few bytes — should share storage.
  Agent workflows are iterative: a model retrained with slightly
  different hyperparameters produces weights that are 98% identical to
  the previous run.

- **Integrity verification.** A recipient must be able to verify they
  received exactly the bytes the sender intended. No silent corruption,
  no truncation, no substitution.

- **Efficient transfer.** Sending a 10 GB artifact to another machine
  should transfer only the bytes the recipient lacks, not the entire
  file.

Content-addressable storage provides all three from a single design
choice: name content by its cryptographic hash. Identical content
produces identical names (dedup). The hash verifies integrity. And
chunking content into hash-addressed pieces means transfer can skip
pieces the recipient already has.

Matrix is not suitable for artifact storage. State events are capped at
65 KB. Homeservers are not designed for multi-GB blobs or millions of
objects. The artifact service is a Bureau service principal with its own
local persistence. Matrix provides coordination and metadata discovery
— which rooms have artifact service bindings, which machine hosts a
given artifact. The service's own storage handles the data path.

---

## Hashing

All artifact hashes are 32-byte BLAKE3 digests computed in keyed mode.
BLAKE3's keyed mode takes a 32-byte key and produces a hash that is
cryptographically independent from the hash of the same input under a
different key. Bureau uses this for domain separation: three fixed keys
ensure that the same input bytes produce different hashes in different
contexts.

The three domains are:

- **Chunk domain** (`bureau.artifact.chunk`): hashes the uncompressed
  bytes of a single chunk. These hashes appear in container indexes and
  drive deduplication.

- **Container domain** (`bureau.artifact.container`): hashes the Merkle
  root of a container's chunk hashes. Used to address containers on disk
  and in transfer protocols.

- **File domain** (`bureau.artifact.file`): hashes the Merkle root of
  all chunk hashes across the entire artifact. This is the artifact's
  identity — the hash that appears in references, metadata, and
  reconstruction records.

Domain keys are the ASCII domain name zero-padded to 32 bytes. Using
readable ASCII makes the keys inspectable in hex dumps and debuggers
without sacrificing any cryptographic property — BLAKE3 keyed mode
treats the key as an opaque 32-byte value.

Domain separation prevents cross-domain collisions: an attacker cannot
craft a chunk whose hash collides with a container hash, because the two
are computed under different keys. This is important for a system that
stores untrusted content — an agent could try to produce a chunk whose
hash matches a container hash to confuse the storage layer.

### Merkle Trees

Multi-chunk artifacts use a binary Merkle tree over chunk hashes to
produce the root hash for the file domain. The tree is constructed
bottom-up: adjacent pairs of hashes are concatenated and hashed with the
chunk domain key. If a level has an odd number of nodes, the last node
is promoted without hashing (not duplicated — duplicating would mean two
different inputs produce the same root when one is a prefix of the
other).

The Merkle structure enables verified streaming: a recipient can verify
any sub-range of chunks against the file hash by checking the relevant
branch of the tree, without downloading the entire artifact.

### Artifact References

The file-domain hash is 32 bytes (64 hex characters). For user-facing
contexts, the short form is `art-` followed by the first 12 hex
characters (6 bytes, 48 bits). This provides a 2^48 address space —
roughly 280 trillion distinct references, with collision probability
reaching 1% around 17 million artifacts (birthday bound). When a
collision occurs (two distinct file hashes sharing the same 12-char
prefix), resolution fails with an error listing all colliding full
hashes. The user disambiguates by providing more characters.

---

## Chunking

Content is split into chunks using GearHash content-defined chunking
(CDC). A 64-bit rolling hash advances byte-by-byte through the content.
When the hash satisfies a boundary condition (the top 16 bits are zero,
giving a probability of roughly 1/65536 per byte), a chunk boundary is
placed.

The parameters are:

- **Target chunk size**: 64 KiB (the expected chunk size given the mask
  probability)
- **Minimum chunk size**: 8 KiB (no boundary is recognized in the first
  8 KiB of a chunk, preventing tiny fragments)
- **Maximum chunk size**: 128 KiB (a boundary is forced if no natural
  boundary occurs within this range)

The minimum chunk size enables a skip-ahead optimization: the first
`min - 65` bytes of each chunk cannot contain a boundary (the rolling
hash window is 64 bytes), so the GearHash computation skips directly
past them.

Content-defined chunking is the reason deduplication works across
similar files. When a few bytes are inserted or deleted from a file,
fixed-size chunking shifts all subsequent chunk boundaries — every
chunk after the edit is different, even though the content is nearly
identical. CDC boundaries are determined by the local byte pattern, so
insertions and deletions only affect chunks near the change. Two
versions of a file that differ by a small edit share all chunks except
those near the edit point.

### Small Artifact Fast Path

Content under 256 KiB is stored as a single chunk with no CDC overhead.
This covers the majority of agent-produced artifacts: stack traces,
screenshots, small configs, test outputs, conversation snippets. For
these, the file-domain hash is simply `HashFile(HashChunk(content))` —
one chunk hash wrapped in the file domain.

---

## Compression

Each chunk is compressed independently using one of four codecs:

| Tag | Algorithm | Use case |
|-----|-----------|----------|
| 0 | None | Already-compressed content (PNG, video, compressed archives) |
| 1 | LZ4 | Fast, moderate ratio (~1.5-2x). General purpose. |
| 2 | Zstd level 3 | Text-heavy content (~3-5x). JSON, logs, source code. |
| 3 | BG4+LZ4 | Float32 tensor data. Byte-group transpose then LZ4. |

Chunk hashes are always computed on uncompressed bytes. This means
deduplication works across compression algorithm changes — re-
compressing an artifact with a different codec produces different
compressed chunks but identical chunk hashes, so the reconstruction
record (and file hash) remain the same.

### Auto-Selection

The compression algorithm is selected once per artifact by probing the
first chunk with zstd:

- Ratio >= 1.5x → zstd for all chunks
- Ratio >= 1.1x → LZ4 for all chunks
- Ratio < 1.1x → no compression

Content-type hints short-circuit the probe: text, JSON, XML, and SQL
default to zstd. Safetensors default to BG4+LZ4.

### Per-Chunk Fallback

Even with a globally selected algorithm, individual chunks may fall back
to no compression if the selected algorithm produces output larger than
the input (incompressible data within an otherwise compressible file).
The compression tag is stored per-chunk in the container index, so the
reader decompresses each chunk with the correct algorithm.

### BG4+LZ4 for Tensor Data

Adjacent float32 weights in neural networks tend to have similar
exponents and sign bits, but relatively random mantissa bits. The BG4
(byte-group-4) transform rearranges bytes by position within the
4-byte float: all byte-0s together, all byte-1s, all byte-2s, all
byte-3s. The exponent bytes (byte 3) become a highly compressible
sequence of similar values. LZ4 then compresses the transposed buffer.
This achieves 2-4x compression on typical model weights where standard
compressors achieve barely 1.1x.

---

## Containers

Chunks are aggregated into containers — binary files holding up to ~1024
compressed chunks in ~64 MiB of data. Containers are the unit of disk
storage, network transfer, cache eviction, and garbage collection.

The alternative — storing each chunk as a separate file — breaks down at
scale. A 1 TB dataset at 64 KiB chunks produces roughly 16 million
chunks. Storing each as a file exhausts inodes and makes directory
traversal unacceptably slow. Containers batch ~1024 chunks into a single
file, reducing inode pressure by three orders of magnitude.

### Container Format

The container format is a fixed-layout binary structure designed for
single-I/O index reads and arithmetic offset computation:

```
┌──────────────────────────────────────────────────────────────────┐
│ Magic (8 bytes): "BUREAU" + version byte (0x01) + reserved (0x00)│
├──────────────────────────────────────────────────────────────────┤
│ Chunk count (4 bytes, little-endian uint32)                      │
├──────────────────────────────────────────────────────────────────┤
│ Chunk index: count × 48-byte entries                             │
│   ┌─ Chunk hash (32 bytes)                                       │
│   ├─ Compression tag (1 byte)                                    │
│   ├─ Reserved (3 bytes, zero)                                    │
│   ├─ Compressed size (4 bytes, LE uint32)                        │
│   ├─ Uncompressed size (4 bytes, LE uint32)                      │
│   └─ Reserved (4 bytes, zero)                                    │
├──────────────────────────────────────────────────────────────────┤
│ Compressed chunk data (variable length, concatenated)            │
└──────────────────────────────────────────────────────────────────┘
```

The chunk index precedes the data. A single read of the header plus
index (a few kilobytes for a full container) provides the complete chunk
catalog. The byte offset of any chunk is computed arithmetically:
`header_size + index_size + sum(compressed_sizes[0..i-1])`. No seeking
through data to locate a chunk's position.

The container hash is the container-domain BLAKE3 hash of the Merkle
root over its chunk hashes. Identical chunks packed in the same order
produce the same container hash, enabling container-level dedup: if two
artifacts chunk identically (which happens when the same content is
stored again), the resulting containers are identical and only stored
once.

Container size limits are soft: the last chunk that pushes a container
past the count or size limit is included, then the container is flushed.
This avoids splitting a single chunk across containers.

---

## Reconstruction

A reconstruction record maps a file-domain hash to the ordered sequence
of containers and chunk ranges needed to reassemble the original content.
It is the bridge between an artifact's identity (its file hash) and the
physical storage of its bytes (containers on disk).

The record is CBOR-encoded (RFC 8949, Core Deterministic Encoding) and
contains:

- **Version**: format version (currently 1)
- **File hash**: the artifact's file-domain hash
- **Size**: total uncompressed content size in bytes
- **Chunk count**: total number of chunks across all containers
- **Segments**: ordered list of (container hash, start index, chunk
  count) tuples

Each segment represents a contiguous run of chunks within a single
container. A single-container artifact has one segment. A large artifact
spanning three containers has three segments. Range-based representation
is compact: most artifacts pack their chunks into containers in order,
so a single segment covers an entire container. A 1 GB artifact with 16K
chunks across 16 containers is described by 16 segments, not 16K
individual chunk references.

Reconstruction proceeds by iterating segments in order, reading each
container's index to locate the specified chunk range, decompressing each
chunk, and concatenating the results. After reconstruction, the file
hash is recomputed from all chunk hashes via the Merkle tree and verified
against the record's file hash.

---

## On-Disk Layout

The artifact store uses a flat root directory with subdirectories for
each data type. Each subdirectory uses two-level hex sharding: the first
two hex characters of the hash form the first directory level, the next
two form the second. With 100K artifacts (~300K files across containers,
reconstruction records, metadata, and tags), each shard directory
contains a few hundred entries — well within filesystem readdir
performance.

```
<store-root>/
├── containers/
│   └── a3/f9/a3f9b2c1e7d4f8a9b0c1d2e3f4a5b6c7d8e9f0a1b2c3d4e5f6a7b8c9d0e1
├── reconstruction/
│   └── b1/c2/b1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0.cbor
├── metadata/
│   └── b1/c2/b1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0.cbor
├── tags/
│   └── 4a/05/4a05bbae1f2d3e4c5b6a7988d0c1f2e3a4b5c6d7e8f9a0b1c2d3e4f5a6b7.cbor
└── tmp/
```

All writes use an atomic temp-file-then-rename pattern: data is written
to a file in `tmp/`, and once the write is complete, the file is renamed
to its final sharded path. This guarantees that readers never see a
partially-written file. The rename is atomic on POSIX filesystems (same
filesystem, which the single store root ensures).

---

## Metadata and Indexing

Each artifact has a metadata record separate from its reconstruction
record. The reconstruction record describes the physical layout
(containers, segments, chunk ranges); the metadata record describes the
application-level properties:

- **Content type**: MIME type (e.g., `application/octet-stream`,
  `text/plain`, `application/x-safetensors`)
- **Filename**: original filename, if provided by the uploader
- **Description**: human-readable summary
- **Labels**: arbitrary string tags for categorization
- **Cache policy**: controls eviction and replication behavior
- **Visibility**: `private` or `public` (see Encryption and Visibility)
- **TTL**: time-to-live for automatic expiration
- **Size, chunk count, container count, compression**: computed at store
  time

Metadata is stored as sharded CBOR files on disk, parallel to
reconstruction records. At service startup, all metadata files are
loaded into an in-memory index with secondary indexes by content type,
label, cache policy, and visibility. Queries that filter by these
dimensions start with the smallest matching index and intersect
candidate sets.

### Artifact References and Tags

The short reference (`art-` + 12 hex chars) is built from a scan of
metadata filenames at startup — no file reads required, just directory
walking. When two distinct file hashes share the same 12-char prefix
(which becomes likely around 17 million artifacts), the ref index tracks
all colliding hashes and resolution returns an error listing them.

Tags are mutable named pointers to immutable content. Tag names are
hierarchical, with slashes as directory separators (e.g.,
`pipeline/build/latest`, `model/production/weights`). Tags support
compare-and-swap: the default write mode requires the caller to supply
the expected previous target hash, failing if another writer has
modified the tag since it was read. An optimistic (last-writer-wins)
mode is available for use cases where conflict detection is unnecessary.

---

## Caching

The cache is a bounded, self-cleaning container cache that accelerates
repeated reads without unbounded growth. Containers are the cache unit
— not chunks (which would fragment the cache and require ~1000x more
entries) and not files (which would couple the cache to the
reconstruction layer).

The cache has three components:

### Device

A fixed-size file, memory-mapped read-only. The size is set at creation
and does not grow. Reads go through the memory map (zero-syscall I/O via
the kernel page cache). Writes use positioned writes (`pwrite`) to
specific offsets. The device file is self-contained: moving it to
another path or another machine produces a working cache.

### Block Ring

The device is divided into fixed-size blocks (256 MiB by default). A
circular write cursor advances through blocks sequentially. When a
container is cached, it is written at the current write position; when
the position reaches the end of a block, the cursor advances to the next
block. When the cursor wraps around to a block that was previously
written, that block is reclaimed — all index entries pointing to it
become stale.

Staleness is detected via a generation counter: each time the write
cursor wraps past a block, the block's generation increments. An index
entry records the generation at write time; on read, the stored
generation is compared to the block's current generation. A mismatch
means the block has been overwritten.

This eviction model has no per-access bookkeeping. LRU requires
move-to-front operations on every read, adding write amplification and
lock contention. The ring is append-only: writes are sequential (good
for SSDs), reads are random (handled by the memory map). The generation
check is a single atomic compare on the read hot path — no lock
required.

### Index

An in-memory hash map from container hashes to (block, offset, length,
generation) tuples. Backed by an append-only log file on disk. New
entries are appended to the log as fixed-size 64-byte records; stale
entries (from reclaimed blocks) are pruned during periodic compaction,
which rewrites the log with only live entries.

### Pin Store

Containers in the pin store are exempt from ring eviction. They are
stored as separate files in a sharded directory alongside the ring
device. Pinning is used for artifacts with a `pinned` cache policy or
for containers that must survive indefinitely (e.g., artifacts backing a
FUSE mount).

---

## Transfer Protocol

The artifact service communicates over Unix sockets using a protocol
that interleaves length-prefixed CBOR messages with raw binary data
streams.

### Message Framing

Every CBOR message is preceded by a 4-byte big-endian uint32 length
prefix. The receiver reads the length, then reads exactly that many
bytes of CBOR. This avoids the CBOR stream decoder's read-ahead
buffering, which would consume bytes from the binary data stream that
follows a header.

### Wire Layouts

Upload (store):
```
[4B length][CBOR StoreHeader][binary data stream][4B length][CBOR StoreResponse]
```

Download (fetch):
```
[4B length][CBOR FetchRequest][4B length][CBOR FetchResponse][binary data stream]
```

### Binary Data Transfer Modes

Two modes, selected by the sender:

- **Sized transfer**: the header specifies the content length. The
  receiver reads exactly that many raw bytes after the header. No
  framing overhead.

- **Chunked transfer**: the content length is unknown. The sender writes
  length-prefixed frames (4-byte header + up to 1 MiB of data per
  frame). A zero-length frame signals end-of-stream. The receiver
  reassembles frames transparently.

### Small Artifact Optimization

Content under 256 KiB is embedded directly in the CBOR header as a byte
string field. No binary data stream at all — the entire upload or
download is a single CBOR message round-trip. CBOR byte strings transfer
without base64 encoding, so there is no serialization overhead beyond
the length prefix.

### Push Targets

A store request can include push targets: machine names to replicate the
artifact to after successful local storage. The store always succeeds
locally first; push results are reported per-target in the response.
This enables asynchronous replication without blocking the store path.
Push failures are reported but do not fail the store.

---

## FUSE Mount

The artifact FUSE filesystem provides transparent filesystem access to
artifacts without explicit fetch commands. An agent can `cat` a tagged
artifact, `cp` a file into the mount to store it, or memory-map a large
artifact for random access.

### Two Namespaces

The mount tree has two top-level directories:

```
<mount>/
├── tag/
│   ├── pipeline/build/latest         → tagged artifact content
│   ├── model/production/weights.bin  → tagged artifact content
│   └── ...
└── cas/
    ├── a3f9b2c1e7d4...              → content by full hash
    └── ...
```

**`tag/`** exposes the tag hierarchy. Slash-separated tag names become
directory levels. Leaf entries are regular files whose content is the
tagged artifact. Reading a tag file streams the artifact content.
Writing to a tag file stores new content and atomically updates the tag
via compare-and-swap.

**`cas/`** exposes the content-addressed namespace. Entries are looked up
by full hex hash. The directory is not listable (the keyspace is too
large for readdir). Reading a CAS entry streams the artifact content.
Writing a CAS entry stores the content and verifies the resulting file
hash matches the filename.

### Read Path

On first open, the FUSE handler loads the reconstruction record and
builds a chunk table mapping byte offsets to (container, chunk index)
tuples. Read requests binary-search the chunk table for the target
offset, decompress the relevant chunk, and copy the requested byte
range. The first access prefetches the next several containers in
reconstruction order to hide I/O latency for sequential reads.

Artifact content is immutable, so the kernel is instructed to cache
read data across file descriptor lifetimes. Subsequent reads of the same
offset serve from the kernel page cache with no userspace round-trip.

### Write Path

Creating or opening a file for write returns a write handle that buffers
all data in memory. On close, the buffered content is passed through the
full CAS pipeline (chunk, compress, container, disk). For tag paths, the
tag is updated atomically via compare-and-swap — if a concurrent writer
modified the tag since the file was opened, the store succeeds (the
content is persisted) but the tag update fails, signaling the conflict.

---

## Encryption and Visibility

The artifact store on a Bureau machine is plaintext. Filesystem
permissions and sandbox isolation protect local data — the same model as
any Unix system with proper access controls. Encryption enters the
picture at the boundary: when artifacts leave the Bureau network for
external storage (S3, cloud backup, shared cache between deployments),
private content must be confidential.

The threat model is an adversary with read access to the external
storage backend. This includes the storage provider itself, any
administrator with bucket access, and anyone who compromises the storage
credentials. The adversary should learn nothing about artifact content,
filenames, content structure, or which artifacts exist in the Bureau
deployment. The residual leakage — the number and approximate sizes of
encrypted blobs — is inherent to any scheme that encrypts blobs
individually and is acceptable for this threat model.

### Visibility Levels

Every artifact has a visibility level set immutably at store time:

- **Private** (the default): content, filename, and content type are
  encrypted before any transfer outside the Bureau network. The
  artifact's content-addressed hash is obscured in external storage
  keys, preventing content correlation across deployments.

- **Public**: not encrypted for external storage. Used for content from
  public sources (open-source dependencies, public datasets, published
  model checkpoints) where encryption adds storage and CPU cost without
  security benefit. Still integrity-verified via the content hash.

Empty visibility normalizes to private. The secure default means an
agent that forgets to specify visibility — or a pipeline that doesn't
think about it — stores content encrypted. Opting into public is a
conscious choice that requires the caller to explicitly declare the
content is not sensitive.

Two levels rather than a richer ACL because visibility answers a single
question: is this content encrypted when it leaves the machine?
In-network access control — which principals can read which artifacts —
is handled by service identity tokens and room-scoped service bindings
(see authorization.md). Visibility is orthogonal to access control.

### Key Derivation

The encryption system derives all keys from a single deployment master
key through two independent paths. The two-path structure is critical:
it preserves container-level deduplication in external storage while
giving each artifact its own encrypted reconstruction record.

#### Deployment Master Key

A 32-byte random secret, provisioned once per Bureau deployment and
shared across all machines. It is distributed to each machine's launcher
via the credential provisioning system (see credentials.md): encrypted
with age to the machine's public key, stored as a state event in the
machine's configuration room, decrypted at boot by the launcher. The
launcher holds it in guarded memory — allocated outside the Go heap via
mmap, locked into physical RAM, excluded from core dumps, zeroed on
release.

At artifact service startup, the launcher derives the artifact
encryption key via HKDF and passes it to the service process via stdin
(see architecture.md for the standard credential delivery pattern). The
service reads the key into guarded memory and closes stdin. The key is
never written to disk, included in logs, or exposed in error messages.

#### Path 1: Per-Artifact Keys (Reconstruction Records)

Each artifact gets a dedicated key tree rooted in its file hash:

```
per_artifact_key     = HKDF-SHA256(ikm = deployment_master_key,
                                    info = "bureau.artifact.v1" || file_hash)

recon_encryption_key = HKDF-SHA256(ikm = per_artifact_key,
                                    info = "bureau.artifact.recon.enc.v1")

obscured_recon_ref   = BLAKE3-Keyed(key = per_artifact_key,
                                     data = "bureau.artifact.ref.recon.v1")
```

The `recon_encryption_key` encrypts the reconstruction record for
external storage. The `obscured_recon_ref` is the opaque identifier
under which the encrypted record is stored. Both derive from the
per-artifact key, which itself derives from the deployment master key
and the file hash.

#### Path 2: Per-Container Keys (Deployment-Wide)

Container keys derive directly from the deployment master key and the
container hash, independent of which artifact references the container:

```
container_encryption_key = HKDF-SHA256(ikm = deployment_master_key,
                                        info = "bureau.artifact.container.enc.v1"
                                               || container_hash)

obscured_container_ref   = BLAKE3-Keyed(key = deployment_master_key,
                                         data = "bureau.artifact.ref.container.v1"
                                                || container_hash)
```

The same container always produces the same obscured reference and the
same encryption key, regardless of which artifact it belongs to. This is
what preserves deduplication in external storage: before encrypting and
uploading a container, the service checks whether the obscured reference
already exists in the storage backend. If it does, the upload is
skipped. If two artifacts share containers (which is common — it is the
whole point of the CAS design), those containers are stored once in the
external backend.

The alternative — deriving container keys from the per-artifact key —
would mean the same container encrypted for two different artifacts
produces different ciphertext and different storage keys. External
storage would hold duplicate copies of identical data, defeating the
deduplication that the CAS pipeline carefully preserves locally.

#### Why HKDF-SHA256

HKDF is the standard key derivation function for deriving encryption
keys from a master secret (RFC 5869). Using SHA-256 as the hash
function (rather than BLAKE3) separates the key derivation domain from
the content hashing domain. Even a theoretical weakness in BLAKE3 used
as a KDF would not affect the encryption keys, and vice versa. HKDF-
SHA256 is also the most widely analyzed and interoperable choice, which
matters for the encryption subsystem where conservatism is appropriate.

#### Why XChaCha20-Poly1305

XChaCha20-Poly1305 is an authenticated encryption with associated data
(AEAD) construction with a 24-byte nonce. The 24-byte nonce eliminates
nonce management concerns: with random nonces, the birthday bound is
2^96 encryptions per key. Since each container key is used once per push
(and even aggressive re-pushing stays astronomically far from 2^96),
nonce reuse is not a practical concern. XChaCha20-Poly1305 is the AEAD
primitive used internally by age, so no additional cryptographic
dependency is introduced.

#### Why Not age for Symmetric Encryption

age is designed for recipient-oriented encryption: you encrypt *to* a
public key, and the holder of the corresponding private key decrypts.
External storage encryption has no recipient. The artifact service
encrypts for itself — to decrypt later when the content is fetched back.
age's symmetric mode (passphrase-based) uses scrypt for key derivation,
which is deliberately expensive (scrypt is designed to resist brute-
force attacks on weak passwords). Bureau's symmetric keys are 32 bytes
of cryptographic randomness derived via HKDF — the scrypt cost is
wasted. Raw AEAD with HKDF provides equivalent confidentiality with
deterministic, fast key derivation.

age is used for its intended purpose: recipient-based sharing (see
Recipient-Based Sharing below).

#### Why Non-Deterministic Encryption

Each encryption uses a fresh random 24-byte nonce, so encrypting the
same content twice produces different ciphertext. The alternative —
deterministic encryption (e.g., SIV mode, or deriving the nonce from
the content) — produces identical ciphertext for identical plaintext.
This leaks the equality relation: an adversary monitoring external
storage who sees two identical ciphertext blobs knows the underlying
content is identical, even without decrypting. An adversary with access
to two different Bureau deployments' storage backends could detect when
both store the same file (the same model weights, the same dataset, the
same credential template).

Random nonces make each encryption of the same content produce different
ciphertext. This eliminates cross-storage content correlation.

The cost of non-deterministic encryption is that deduplication cannot be
determined by ciphertext comparison. But Bureau already tracks
deduplication at the CAS layer — containers are identified by their
content hash, and the obscured reference is deterministic (same
deployment key + same container hash = same obscured ref). Dedup
decisions happen before encryption, using hashes, not after encryption,
using ciphertext.

### Encrypted Blob Format

Both containers and reconstruction records use the same envelope:

```
┌─────────────────┬──────────────────────┬─────────────────────────────────────┐
│ Version (1 byte)│ Nonce (24 bytes)     │ Ciphertext + Tag (N + 16 bytes)    │
│ 0x01            │ random, crypto/rand  │ XChaCha20-Poly1305 Seal output     │
└─────────────────┴──────────────────────┴─────────────────────────────────────┘
```

The version byte is included as additional authenticated data (AAD) in
the AEAD Seal/Open call. It is authenticated but not encrypted — the
reader must inspect it before decryption to select the correct key
derivation scheme. Tampering with the version byte causes AEAD
authentication failure.

The file-domain hash (for reconstruction records) or container hash (for
containers) is also included in the AAD. This binds the ciphertext to
the artifact or container identity: an attacker who can modify external
storage cannot swap one encrypted blob for another without causing
authentication failure on decryption.

Total overhead per blob: 1 + 24 + 16 = 41 bytes. For a ~64 MiB
container, this is negligible. For a ~200 byte reconstruction record,
the overhead is roughly 20%, which is acceptable.

### Reference Obscuring

External storage keys for private artifacts use opaque identifiers
derived from BLAKE3 keyed hashing. The key is the deployment master key
(for container refs) or the per-artifact key (for reconstruction record
refs). The input includes a domain tag to prevent reconstruction record
refs from ever colliding with container refs, even if the underlying
hashes happened to match.

Obscured references are:

- **Deterministic** for the same key and hash. This is essential for
  the container dedup check: "does this obscured ref already exist in
  external storage?" must return a stable answer.

- **Opaque** without the key. The obscured ref reveals nothing about the
  underlying hash. An observer cannot recover the file hash or container
  hash from the obscured ref.

- **Deployment-specific.** Different deployments produce different
  obscured refs for the same content, preventing cross-deployment
  correlation.

### External Storage Model

Private and public artifacts occupy separate namespaces in external
storage:

**Private artifacts:**

```
priv/recon/<hex[:2]>/<hex[2:4]>/<obscured_recon_ref>
priv/container/<hex[:2]>/<hex[2:4]>/<obscured_container_ref>
```

Values are encrypted blobs in the format described above.

**Public artifacts:**

```
pub/recon/<hex[:2]>/<hex[2:4]>/<file_hash>
pub/container/<hex[:2]>/<hex[2:4]>/<container_hash>
```

Values are plaintext reconstruction records and container bytes.

The `priv/` and `pub/` prefixes prevent collisions between the two
namespaces. There is no cross-visibility deduplication: a container
referenced by both a public and a private artifact is stored twice (once
plaintext, once encrypted). This is correct — if any artifact referencing
a container is private, the encrypted copy must exist in external
storage regardless of whether the same bytes are also available publicly.

### Metadata Treatment

Nothing leaves the local store except containers and reconstruction
records. Content type, filename, description, labels, cache policy, TTL
— all remain in the local metadata store. The external storage backend
sees:

- Opaque keys (obscured references or hashes)
- Encrypted or plaintext blobs
- Blob sizes (ciphertext = plaintext + 41 bytes for private; exact
  plaintext size for public)
- Blob count (reveals approximate artifact and container counts)

Blob sizes reveal approximate container sizes. This is inherent to
any scheme that encrypts containers individually. Padding containers
to a fixed size would eliminate this but waste storage — padding a
100 KiB container to 64 MiB is unacceptable. The leakage is that an
observer can see "there are N blobs of various sizes," which reveals
approximate total data volume. This is acceptable for the threat model:
the external storage provider already knows how much data it stores.

### Recipient-Based Sharing

When a Bureau deployment shares an artifact with a collaborator (another
Bureau deployment or a specific individual), the encryption model shifts
from symmetric (deployment master key) to asymmetric (age x25519).

The sender constructs a **sharing bundle**: a compact CBOR structure
containing the keys and storage locations needed to fetch and decrypt
the artifact from external storage:

- The file hash (artifact identity)
- The reconstruction record encryption key and obscured storage ref
- For each container: the real container hash, the container encryption
  key, and the obscured storage ref

The sharing bundle is encrypted using age x25519 to the recipient's
public key. The recipient's public key is discovered from their machine's
Matrix state event or provided out-of-band.

The encrypted sharing bundle is small — roughly 100 bytes per container.
A typical artifact with 3 containers produces a ~400 byte sharing
bundle, which after age encryption (adding the age header and x25519
stanza, roughly 200 bytes of overhead) is ~600 bytes. A 10 GB artifact
with 160 containers produces a ~16 KiB bundle. Both are trivially
transmittable via a Matrix message.

The recipient decrypts the sharing bundle with their age private key,
then uses the contained keys and obscured refs to fetch and decrypt each
blob from external storage, and reassembles the artifact through the
standard reconstruction path.

The key insight is: encrypt the keys, not the content. Re-encrypting
gigabytes of container data per recipient would be O(recipients ×
artifact size). Encrypting only the sharing bundle is O(recipients ×
container count × 100 bytes), plus one copy of the encrypted containers
that all recipients share.

### Same-Deployment Pull

Machines within the same Bureau deployment share the deployment master
key (it is distributed to all machines via the credential provisioning
system). Same-deployment pulls do not need sharing bundles — each
machine derives all keys directly from the master key and the artifact's
file hash or container hashes:

1. Machine B knows the file hash (from a Matrix event, a ticket
   attachment reference, a push notification, etc.)
2. B derives the per-artifact key, then the reconstruction record key
   and obscured ref
3. B fetches and decrypts the reconstruction record from external
   storage
4. For each container in the reconstruction: B derives the container
   key and obscured ref from the deployment master key
5. B fetches and decrypts each container
6. B reassembles the artifact and verifies the file hash

### Key Lifecycle

On service startup: the launcher derives the artifact encryption key
from the deployment master key via HKDF and passes it to the artifact
service. The derivation is deterministic — the same master key always
produces the same artifact encryption key. On service restart, the same
key is derived again, and all previously encrypted external storage
remains decryptable.

On deployment master key rotation (an incident response procedure, not
routine): all external storage must be re-encrypted with keys derived
from the new master key. This requires fetching every encrypted blob,
decrypting with the old-key-derived keys, re-encrypting with new-key-
derived keys, uploading to new obscured refs, and deleting old blobs.
The cost is proportional to total external storage size. Key rotation is
expected to be rare — triggered by key compromise, not by calendar.

### Integrity Model

Integrity verification has three layers:

**AEAD authentication** (per-blob): XChaCha20-Poly1305 provides
ciphertext integrity for each encrypted blob. Any modification to the
ciphertext, nonce, or version byte causes decryption to fail. The hash
in the AAD prevents blob swapping.

**Reconstruction record** (manifest integrity): the encrypted
reconstruction record contains the ordered list of real container
hashes. Because the record is AEAD-authenticated, an attacker cannot
add, remove, reorder, or substitute container references without
detection.

**Content hash** (end-to-end): after decrypting all containers and
extracting chunks, the file hash is recomputed from all chunk hashes via
the Merkle tree and verified against the expected file hash. This
catches any mismatch between what the reconstruction record claims and
what the containers actually contain.

These three layers together mean no additional manifest hash is needed.
The AEAD on the reconstruction record guarantees the manifest is
authentic, and the file hash verification guarantees the reassembled
content matches the artifact identity.

### Verification Properties

The encryption implementation must be tested to verify these properties:

- **Plaintext absence.** Encrypting non-trivial content (repeated known
  byte patterns) produces output that does not contain any substring of
  the plaintext. This catches catastrophic failures: encryption silently
  skipped, the AEAD producing identity output, or nonce handling errors.

- **Non-determinism.** Two encryptions of identical content with the
  same key produce different output. This verifies the nonce is actually
  random (not hardcoded, not derived from content, not zero).

- **Round-trip fidelity.** Encrypt then decrypt recovers the original
  bytes exactly for every artifact size (empty-ish to multi-container).

- **AAD binding.** Decryption fails if the ciphertext is moved to a
  different artifact's storage key (wrong hash in AAD).

- **Key isolation.** A key derived for one file hash does not decrypt
  ciphertext encrypted under another file hash's derived key.

- **Visibility boundary.** The same content stored as both private and
  public produces encrypted output for private and plaintext for public.
  Both paths must be exercised in the same test to catch inverted
  conditionals.

- **Egress-only.** Storing an artifact and reading it back locally
  (within the Bureau network) never involves encryption. The local read
  path returns identical bytes to what was stored, with no encryption
  overhead.

---

## The Artifact Service

The artifact service is a Bureau service principal with a Matrix
account, a unix socket API, and service registration in
`#bureau/service`. It is scoped by room membership: the service joins
rooms that declare an artifact service binding, and manages artifacts
for those rooms.

### Write Path

A client connects to the artifact service socket, sends a store header
(content type, filename, visibility, labels, cache policy, push
targets), and streams the content. The service:

1. Reads the content (embedded in the header for small artifacts, or
   from the binary stream for large ones)
2. Chunks the content via CDC (or the small-artifact fast path)
3. Compresses each chunk with the selected codec
4. Packs chunks into containers and writes them to disk
5. Writes the reconstruction record
6. Writes the metadata record
7. Updates the in-memory indexes (ref index, artifact index)
8. Creates or updates a tag if requested
9. Pushes to targets if requested (encrypting for external storage as
   described above)
10. Sends the store response (ref, hash, size, chunk count, compression,
    push results)

### Read Path

A client sends a fetch request with an artifact reference (short ref,
full hash, or tag name) and optional byte range. The service:

1. Resolves the reference to a full file hash (tags resolve to hashes,
   short refs resolve via the ref index)
2. Reads the reconstruction record
3. Iterates segments, reads containers, decompresses chunks
4. Streams the content to the client (embedded in the response for small
   artifacts, or as a binary stream for large ones)

### Service API

Simple request/response actions (single CBOR message round-trip):

- **status**: unauthenticated liveness check
- **exists**: check whether an artifact reference resolves to stored
  content
- **show**: return full metadata for an artifact
- **reconstruction**: return the reconstruction record (segments,
  container references)
- **list**: filtered artifact query (by content type, label, cache
  policy, visibility, size range, with pagination)
- **resolve**: resolve a reference (short ref, tag, or hash) to a full
  hash
- **tag**, **delete-tag**, **tags**: mutable tag management
- **pin**, **unpin**: mark artifacts as protected from garbage
  collection and pin their containers in the cache
- **gc**: trigger mark-and-sweep garbage collection (with dry-run mode)
- **cache-status**: store and cache statistics

Streaming actions (CBOR header + binary data):

- **store**: upload artifact content
- **fetch**: download artifact content (with optional byte-range)

---

## Garbage Collection

Garbage collection is mark-and-sweep, triggered explicitly via the `gc`
action. The live set is the union of:

- All artifacts referenced by at least one tag
- All artifacts with `pinned` cache policy
- All artifacts with an unexpired TTL

Everything outside the live set is collectible. The sweep removes
reconstruction records, metadata files, and any containers not
referenced by any surviving reconstruction record. Dry-run mode reports
what would be collected without deleting.

GC runs under the write mutex, so no concurrent stores or tag updates
can create a race between marking and sweeping.

---

## Room Service Binding and Matrix Integration

Rooms opt into artifact service via `m.bureau.room_service` state events
with state key `"artifact"`. The state event identifies which machine's artifact
service is responsible for the room. The daemon resolves the service
socket at sandbox creation time and bind-mounts it into the sandbox at
a well-known path.

Artifact configuration per room (service principal identity, tag
namespace scoping) uses the `m.bureau.artifact_scope` state event.
The artifact service monitors these events via its own Matrix `/sync`
loop, accepting room invitations and tracking which rooms it manages.

---

## Relationship to Other Design Documents

- **fundamentals.md**: artifacts are a service built on the five
  primitives — sandbox isolation protects the local store, the proxy
  authenticates artifact service requests, observation lets operators
  watch artifact operations in real time.

- **architecture.md**: the artifact service is a Bureau service — unix
  socket, CBOR protocol, service identity tokens, daemon-managed
  lifecycle.

- **information-architecture.md**: `m.bureau.room_service` and
  `m.bureau.artifact_scope` state events use the standard room service
  binding model defined there.

- **credentials.md**: the deployment master key is provisioned and
  distributed through the credential system. age encryption is used for
  the sharing bundle (recipient-based sharing).

- **tickets.md**: ticket attachments reference artifacts via `art-`
  refs. Large content (logs, screenshots, build outputs) lives in the
  artifact service rather than in ticket state events.

- **workspace.md**: workspace agents use the artifact service for build
  outputs, test results, and other workspace-scoped data.

- **fleet.md**: push targets for cross-machine replication are resolved
  through fleet-managed machine discovery and service placement.
