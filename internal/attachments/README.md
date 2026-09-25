# Native Basis Points image attachments

This package implements transparent image attachment upload using the selected
request account and the caller's HTTP transport/proxy. There is no public image
host, port, storage directory, third-party image service, or attachment setting.

## Protocol evidence

Public reference: JaxsonWang/cpa-plugin-oai-basispoints, commit
80027a0db6a56c0f88a54a04308dbc38678f0f97,
internal/basispoints/attachments.go (published September 24, 2026):

- Derive attachments from the same origin and directory as responses_url.
- POST multipart/form-data with one file part containing original image bytes
  and the image MIME type. No purpose field is present in that implementation.
- Use the same Authorization, ChatGPT/OpenAI account, and BPS profile headers.
- Read openai_file_id from the JSON response. Replace image_url with file_id
  in the existing input_image part, retaining its detail.

Source:
https://github.com/JaxsonWang/cpa-plugin-oai-basispoints/blob/80027a0db6a56c0f88a54a04308dbc38678f0f97/internal/basispoints/attachments.go

An unauthenticated GET to the official
https://bps.openai.com/basispoints/api/attachments endpoint returned HTTP 405
with 'Invalid method for URL (GET /attachments)'. This confirms a route exists;
it does not verify authenticated upload, file-ID acceptance, or visual accuracy.
The public reference's local HTTP and streaming tests likewise use fixtures.
No real account credentials were accessed or used for this implementation.

## Implementation boundaries

All inline images validate before upload. Message content and function/custom
tool output arrays are supported; text and tool arguments are never interpreted
as images. Source maps change only after every upload succeeds. A failed batch
may have already uploaded earlier images to the provider, but does not partially
rewrite the request. The adapter does not invent an upstream deletion endpoint.

Limits are 20 MiB per image, 32 MiB decoded total, 20 inline image occurrences,
64 megapixels per image, and 32 active uploads. Only PNG/JPEG/GIF/WebP are accepted.
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

Local tests cover multipart bytes and headers, tool screenshots, atomic failure,
resource bounds, cache isolation and expiry, cancellation, concurrency, redirects,
and malformed upload responses. They do not prove real upstream image recognition.
