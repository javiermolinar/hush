# Native images capability, version 1

Offer `"images":[1]` in `hello.params.capabilities`. A host must install an image
fitter to select it; a bare Harness, an unoffered connection, and the reference
invocation-private host do not select it. No base frames change. This is separate
from the Pi 1.6 RPC image-service capability.

| Request | Parameters | Result |
| --- | --- | --- |
| `ext.images.submit` | `threadId`, `requestId`, `whenBusy`, `text`, `images` | Native `{submissionId}` receipt |
| `ext.images.get` | `threadId`, `entryId`, `messageIndex`, `blockIndex` | One `{mimeType,data}` |

An image is an object containing a supported MIME type (PNG, JPEG, GIF or
WebP) and canonical standard base64 bytes. Submit accepts one to four images,
at most 5 MiB decoded in the aggregate, and at most 32 KiB of caption per SDK
request. A caption may be empty. Paths, URLs and client MIME parameters are not
accepted. The reference host checks the declared container and bounded
dimensions (8192 per axis and 24 million pixels), decodes and fits to the selected
image-capable model using Durable's `images/fit`, and commits only if all
attachments fit. APNG and animated WebP are not supported. Fit and format
validation happen outside the Harness transaction; the model identity is
checked again in the transaction. The fitted content, not the upload, becomes
the committed user message and provider input.

The same native key and queue rules apply. Commit a new submission and its
receipt in one transaction; a repeated key returns the saved receipt without
another admission. A receipt is not settlement. Where synchronous fitting is
busy, the reference host may return `limit` before admission. Invalid image
content and text-only models return `invalid_request`; a changed model may
return `busy`. No attachment is silently removed from an admitted input.

Subscribe, patches and history still carry only the core `omitted` block with
`kind:"image"`. For committed entries this marker and its surrounding indices
form the reference: use the **viewed** thread ID and the immutable entry ID,
then zero-based indices into its `messages` and that message's `blocks`.
Indices are positions in the respective whole arrays, not image counts.
The server resolves against the viewed thread's visible lineage, including
inherited entries of a fork, not an unrestricted store lookup. A marker without
fetchable data (including an unsupported historical MIME type) remains text.
Responses are capped at 5 MiB decoded; state, history and queue never carry an
image payload. An uncommitted stream marker is not an image reference.

See [images.schema.json](images.schema.json) for request parameters and success
envelopes. Base negotiation, framing and error semantics are unchanged.
