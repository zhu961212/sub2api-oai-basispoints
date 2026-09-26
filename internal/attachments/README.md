# Native Basis Points image attachments

This package validates inline images and uploads user-message attachments using
the selected request account and the caller's HTTP transport/proxy. Tool-result
screenshots retain their original data URLs. There is no public image host,
port, storage directory, third-party image service, or attachment setting.

## Protocol evidence

On September 26, 2026, the official Excel manifest located the frontend below:

- https://bps.openai.com/basispoints/api/office/manifest.xml
- https://bps.openai.com/basispoints/extension/360590d7-f8f9-4d88-bf75-0edfe0a4b9f3/assets/x-square-DUrhLSGN.js

Its Nme path constructs user-message images with file_id. Its lge / Eqr paths
construct tool-result images with image_url data URLs and use nullish defaults
for detail. Version 0.5.23 follows that distinction: upload inline user images,
but preserve inline function/custom tool screenshots. Missing or null detail
becomes auto; explicit low/high remain unchanged.

The initial multipart upload reference follows. Its file-ID conversion is not
applied to tool-result screenshots.

Public reference: JaxsonWang/cpa-plugin-oai-basispoints, commit
80027a0db6a56c0f88a54a04308dbc38678f0f97,
internal/basispoints/attachments.go (published September 24, 2026):

- Derive attachments from the same origin and directory as responses_url.
- POST multipart/form-data with one file part containing original image bytes
  and the image MIME type. No purpose field is present in that implementation.
- Use the same Authorization, ChatGPT/OpenAI account, and BPS profile headers.
- Read openai_file_id from the JSON response. For user messages, replace
  image_url with file_id in the existing input_image part, retaining its detail.

Source:
https://github.com/JaxsonWang/cpa-plugin-oai-basispoints/blob/80027a0db6a56c0f88a54a04308dbc38678f0f97/internal/basispoints/attachments.go

An unauthenticated GET to the official
https://bps.openai.com/basispoints/api/attachments endpoint returned HTTP 405
with 'Invalid method for URL (GET /attachments)'. This confirms a route exists;
it does not verify authenticated upload, file-ID acceptance, or visual accuracy.
The public reference's local HTTP and streaming tests likewise use fixtures.
No real OAuth image-recognition acceptance test is claimed for this fix.

## Implementation boundaries

All inline images validate before upload. Message content and function/custom
tool output arrays are supported; text and tool arguments are never interpreted
as images. Tool screenshots retain their original image_url, but still undergo
complete Base64 byte, MIME, format, dimensions, and resource checks. Source maps
change only after every necessary user-message upload succeeds. A failed batch
may have already uploaded earlier images to the provider, but does not partially
rewrite the request. The adapter does not invent an upstream deletion endpoint.

Limits are 20 MiB per image, 32 MiB decoded total, 20 inline image occurrences,
64 × 1024 × 1024 pixels per image, and 32 active uploads. User images and tool
screenshots share the per-request limits; repeated occurrences count separately.
Expanded conversation history is included in these limits, even when each new
turn adds only one image. Limit errors report the encountered count or decoded
size, identify the offending field, and explain that history/screenshots count.
Only PNG/JPEG/GIF/WebP are accepted. Screenshot-only requests also participate
in the transport's image-request admission and estimated concurrency budget.
Images are not compressed or resized; tool screenshots do not invoke attachments.
Multipart image bytes decode directly into the caller's HTTP transport. Buffers
are bounded and cleared before reuse; no image-sized buffer or local file is
created. The small upload response is limited to 64 KiB.

Up to 512 file IDs are cached for at most 30 minutes from upload. This is a local
reuse cap, not a claim about provider retention or file lifetime. Keyed digests
include the full encoded image, MIME, trusted scope, endpoint, and authentication
identity. An empty scope disables cross-request caching without creating
unusable random-scope entries. Within a request, equal encoded images avoid
repeated HMAC work; fully authenticated cache hits skip repeated base64 scans. Concurrent same-scope
uploads share one operation; waiting requests can cancel independently.

All attachment redirects are rejected. Upstream response bodies and transport
errors are never exposed in returned errors, which use fixed messages and safe
field paths. HTTP error statuses are preserved where applicable.

Local regression coverage includes multipart bytes and headers, preserved tool
screenshots, detail defaults, mixed images, complete byte validation, atomic failure,
resource bounds, cache isolation and expiry, cancellation, concurrency, redirects,
and malformed upload responses. Actual run results are recorded in
docs/release-0.5.23-2026-09-26.md. Local tests do not prove real upstream image
recognition or deployment. A generic 422 alone does not establish the unique
cause of the production rejection.
