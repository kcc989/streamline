# Offline browser-task recordings

The Container now has a bounded offline recording engine in `container/recording`.
It accepts timestamped JPEG frames, schedules the existing full-canvas PNG
annotation format, applies timed opaque masks, mixes supplied narration PCM,
and commits a complete H.264/AAC MP4 to a trusted sink. Existing `/start`, WebM
ingest, preview, RTMPS, and live annotation APIs retain their behavior.

Browser automation, capture approval, authentication, and organization policy
belong to the application. The engine accepts no target-app credentials, media
URLs, filesystem paths, arbitrary drawing expressions, or FFmpeg arguments.

## Existing annotation capability and the minimal extension

Live `session.annotation(png)` already accepts changing RGBA PNG snapshots. The
upstream FFmpeg graph replays annotations at 5 fps; transparent snapshots clear
the overlay. Cursor shapes, click markers, and callouts can all be drawn in those
PNGs by the application. `TestExistingAnnotationGraphRendersTimedCursorAndClick`
proves movement/click at 400 ms and clearing at 800 ms with decoded video pixels
using the existing argument builder. There is no new cursor-specific renderer.

Recordings use the same PNG validation and full-canvas alpha compositing model,
but schedule snapshots by media timestamp rather than wall-clock arrival. Masks
are a separate immutable timeline because asynchronous live annotation updates
cannot guarantee privacy for a particular captured frame.

## Local proof

Use the vendored Go dependencies and locally installed FFmpeg/ffprobe:

```sh
cd container
go test -mod=vendor ./...
go run -mod=vendor ./cmd/recording-demo -output /tmp/synthetic-recording.mp4
ffprobe -v error -show_streams /tmp/synthetic-recording.mp4
```

The demo creates entirely synthetic JPEGs, PNG cursor/click snapshots, a timed
mask, and a tone standing in for narration. It produces a 320x240, 10 fps,
two-second MP4 with 20 H.264 frames and an AAC track starting at zero and lasting
two seconds. The destination must not already exist.

## Internal Container HTTP contract

These routes are internal infrastructure endpoints. A recording ID fences stale
requests; it is **not authentication**. An application adapter must authenticate
every operation, bind each ID to its principal, enforce its policy/quotas, and
keep the Container private. The default `StreamlineSessionDO` rejects these
routes, including percent-encoded aliases, instead of passing them through its
generic fallback. An explicit `handleApplicationRequest` adapter may handle them
after checking identity. No cloud recording adapter is shipped in this slice.

Create with `POST /recordings`, `Content-Type: application/json`:

```json
{
  "width": 1280,
  "height": 720,
  "fps": 10,
  "masks": [
    { "start_ms": 0, "end_ms": 1200, "x": 100, "y": 200, "width": 320, "height": 80 }
  ]
}
```

Returns `201 {"recording_id":"<opaque ID>"}`. Only one recording or streaming
session may reserve the Container at a time. A second create returns `409` and
never evicts the existing owner. All subsequent recording requests require
`X-Streamline-Recording-ID`. JSON rejects unknown fields and extra objects.

| Route | Body and additional headers | Result |
| --- | --- | --- |
| `POST /recordings/frame` | JPEG, `Content-Type: image/jpeg`, `X-Frame-Sequence`, `X-Frame-Timestamp-MS` | `204` |
| `POST /recordings/annotation` | RGBA PNG, `Content-Type: image/png`, same sequence/timestamp headers | `204` |
| `POST /recordings/narration` | Mono PCM16 little-endian at 48 kHz, `Content-Type: application/octet-stream`, same sequence/timestamp headers, `X-Narration-Gain: 0..1000` | `204` |
| `POST /recordings/finalize` | `Content-Type: application/json`, `{"end_ms":2000}` | artifact metadata: `id`, `bytes`, `duration_ms` |
| `GET /recordings/output` | No body; optional HTTP Range | committed `video/mp4` attachment |
| `POST /recordings/cancel` | Empty body | `204`; deletes spool/artifact and releases slot |

Sequences are zero-based and contiguous **independently for each input kind**.
An exact sequence/timestamp/content retry is idempotent; a conflicting replay,
gap, or stale ID returns `409`. Frame/annotation timestamps strictly increase;
the first JPEG must be at zero. Annotation may begin later; it persists until
the next snapshot, with a transparent PNG used to clear it. Narration clip
timestamps may arrive in any order; clip identity also includes its gain.

Capture timestamps are integer milliseconds relative to recording start. The
encoder samples the latest JPEG and PNG at each constant-frame-rate tick, holding
them until a newer timestamp. It never uses request arrival time. Finalization
requires a positive end strictly after the last JPEG, at most 300 seconds and
on an exact output tick (`end_ms * fps` divisible by 1000). Annotation snapshots
outside the chosen end have no effect. Narration extending past the end fails
finalization rather than silently losing speech. Audio gaps are silence; overlaps
sum with fixed-point linear gains and saturate to PCM16 bounds. JPEG source input
has no audio. No speech generation service or account access is required.

Finalization is synchronous and terminal on encoder/probe/sink failure or request
cancellation. The same completed `end_ms` can be retried to recover the same
metadata. Interrupted uploads can reconnect and retry acknowledged sequences.
Interrupted downloads can reopen or resume the committed MP4 with Range. A new
create has no automatic retry/idempotency token; retain the returned ID. There
is no recovery across Container/process restart: ephemeral state is deliberately
bounded, and callers must create a new recording after restart or failure.

## Privacy and publication

Masks are validated and copied at creation; there is no late mask update path.
Every output tick **overlapping** a `[start_ms,end_ms)` mask interval is blackened,
including held JPEGs. Spatial bounds round outward to 16-pixel codec blocks with
one additional guard block, clipped to the canvas. This conservatively covers
fractional frame intervals and chroma/codec edges. The actual coverage can be
larger than the requested rectangle and up to one tick longer at either end.
Masks are applied after PNG annotations; no annotation can reveal covered source
pixels. Applications must determine a complete approved privacy plan before
capture. The engine does not detect sensitive content automatically.

There is no recording preview, raw frame endpoint, or partial MP4 download.
Raw JPEGs and PNGs exist only in an engine-owned private `0700` spool directory
with `0600` files, removed after completion/failure/cancellation. All rendering
and effects must succeed before publication. FFmpeg receives only composited,
masked RGB output frames. A bounded ffprobe check verifies codec, dimensions,
frame count, start time, and video/audio duration before the sink commit. Corrupt
scheduled overlays or failed encoding/probing publish nothing; there is no
fallback to unprocessed output. Error responses contain no source media/path or
process diagnostics.

## Resource and lifecycle budgets

| Resource | Engine/HTTP limit |
| --- | --- |
| Canvas / frame rate | positive even size up to 1920x1080; 1..30 fps |
| Recording timeline | 300 seconds / at most 9000 JPEGs |
| JPEG or PNG body | 2 MiB, decoded dimensions must match the canvas |
| Annotation timeline / masks | 1000 snapshots / 64 masks |
| Narration | 16 clips, 2 MiB per clip, 32 MiB total |
| All ingested JPEG/PNG/PCM combined | 128 MiB |
| MP4 | strictly below 128 MiB |
| JSON / body read | 64 KiB / 10 seconds |
| Mutations | one admitted HTTP body/operation; no queued work; `429` + `Retry-After: 1` |
| Render | 5-minute timeout, fixed encoder threads; cancellation kills and waits for encoder |
| Download | one download, 30-second write deadline, interrupted on cancellation |
| Slot lifetime | 15 minutes from creation, includes final artifact retrieval |

The spool can additionally contain at most 28.8 MiB of mixed PCM and one bounded
temporary encoded MP4. Pixel memory is bounded to a source image, annotation
image, compositing canvas and one RGB frame; no decoded frame queue is retained.
PCM clip buffers are released at finalization/cancellation. Cleanup waits for an
active download before releasing the slot, preventing retained artifact growth.
Applications should use streaming requests/responses and bound caller retry
queues; they may apply tighter budgets before reaching this engine.

## Sink boundary and followups

`recording.Sink.Commit(ctx, artifact, reader)` receives a complete verified MP4.
Implementations must atomically publish the entire artifact or return an error
without publishing any bytes. Sink objects are trusted server configuration;
clients cannot select paths or credentials. `LocalSink` copies into a private
temporary file, verifies byte counts, syncs, and links atomically without
overwriting an artifact. Its output is ephemeral and deleted on cancellation or
slot expiry. An application's durable object-storage/R2 sink would own object
keys, authorization, retention, and download capabilities. R2 integration is a
followup, not a dependency or durability claim of this implementation.

Captions, chapters, explicit holds, zooms, live masked preview, cloud adapters,
and durable restart/recovery are followups. The existing live annotation and
subtitle APIs remain available for their current supported inputs.
