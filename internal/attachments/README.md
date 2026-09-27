# Native Basis Points image and document attachments

This package validates inline images and uploads user-message attachments using
the selected request account and the caller's HTTP transport/proxy. Tool-result
screenshots retain their original data URLs. There is no public image host,
port, storage directory, third-party image service, or attachment setting.
Inline PDF, DOC, and DOCX input_file blocks use the same native attachment
transport and become file_id references in message and typed tool-result arrays.

## Protocol evidence

On September 26, 2026, the official Excel manifest located the frontend below:

- https://bps.openai.com/basispoints/api/office/manifest.xml
- https://bps.openai.com/basispoints/extension/360590d7-f8f9-4d88-bf75-0edfe0a4b9f3/assets/x-square-DUrhLSGN.js

The manifest and this bundle were checked again on September 27, 2026. The
manifest also advertises the Word Document host. The bundle provides these
document-specific protocol anchors (minified function names are build-specific):

- dOt creates FormData, calls append("file", e.file, e.file.name), and POSTs to
  path "attachments" with the normal authentication configuration. No purpose
  field is added. fOt reads response.openai_file_id, filename, and content_type.
- Nme emits {type:"input_file",file_id:a} for ordinary non-image attachments.
  Its alternate k$r path is used when a non-image file is larger than 32 MiB or
  reported aggregate input tokens reach modelAutoCompactTokenLimit. That path
  uses uploadedFileIds; d$r places their JSON array in attachment_file_ids and
  removes inline file attachments. The bounded local adapter uses the ordinary
  file-ID path and does not claim to implement the token-based fallback.
- The accepted MIME and extension maps include application/pdf (.pdf),
  application/msword (.doc), and
  application/vnd.openxmlformats-officedocument.wordprocessingml.document
  (.docx). The RDe validation path checks file headers, including PDF and ZIP
  signatures, before uploading. These public bundle anchors confirm the native
  upload/file-ID representation; they do not verify a live authenticated turn.

The bundle does not contain file_data. Inline Responses file_data is therefore
decoded and uploaded by this adapter, never blindly forwarded upstream.

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

Limits are 20 MiB per image, 32 MiB decoded total, 128 inline image occurrences,
64 × 1024 × 1024 pixels per image, and 32 active uploads. User images and tool
screenshots share the per-request limits; repeated occurrences count separately.
Expanded conversation history is included in these limits, even when each new
turn adds only one image. The count is a local resource safeguard, not a provider
capability guarantee; the adapter never drops or compresses historical images.
Limit errors report the encountered count or decoded
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

## Document implementation boundaries

RewriteFiles accepts base64 data URLs with one of the three document MIME types,
or raw base64 plus a matching .pdf/.doc/.docx filename. A data URL without a
filename gets attachment.pdf, attachment.doc, or attachment.docx. Names may
contain Unicode, but must be valid UTF-8, at most 255 bytes, and contain no path
separators or control characters. MIME, extension, and file signature must
agree. The signatures checked are %PDF-, OLE Compound File for .doc, and the
ZIP local-file header for .docx; this is not a full PDF, OLE, or OOXML parser.

Document limits are 20 MiB decoded per file and 20 inline file occurrences per
request; images retain their independent 20-occurrence limit. Images and files
share one 32 MiB decoded request budget. Repeated attachments and conversation/
tool history count toward these limits. ValidateMixedInputs checks every inline
image and document before either upload path runs, without network, cache, or
source mutation, so an invalid PDF cannot trigger an earlier image upload.
Existing native file IDs are preserved. Remote
file_url input is rejected; this adapter does not fetch arbitrary document URLs.

All documents validate before upload, and inline file_data/filename fields are
replaced only after all document uploads succeed. Streaming multipart preserves
the exact original bytes, MIME, and safe filename. The shared bounded cache and
upload limits apply; document digests additionally include a separate document
namespace and filename, preserving same-content/different-name semantics.
File contents, filenames, and unredacted upstream bodies are never placed in
error messages or retained in the persistent cache. No local file is created.

Document regressions cover PDF/DOC/DOCX multipart bytes and headers, native ID
preservation, message/function/custom-tool content, MIME and filename mismatch,
invalid base64 and signatures, request limits, cache identity and filename
isolation, cancellation, redirects, atomic source preservation on failure, and
mixed image/document preflight and shared byte limits.
No paid upstream request or authenticated PDF/Word acceptance test is claimed.
