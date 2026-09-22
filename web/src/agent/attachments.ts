import type { AttachmentAdapter, CompleteAttachment, PendingAttachment } from '@assistant-ui/react'
import { idem, request } from '../api'

// The image attachment adapter (doc/chat-features.md §4.2).
//
// SimpleImageAttachmentAdapter is deliberately NOT used: it turns a photo into a data URI inside the
// message, which would put the bytes in AgentMessagePart, in agent_run_chunks and in the libfx
// checkpoint, and re-send them to the model on every turn. This adapter uploads once and puts a reference
// in the message, so the bytes live in one place and the server can ownership-check every use (§4.4).

/** The whitelist mirrors the server's (§4.5). The server sniffs the bytes anyway; this only saves the user
 *  an upload that was always going to be refused. */
export const IMAGE_ACCEPT = 'image/png,image/jpeg,image/webp,image/gif'

type UploadResponse = {
  attachment_id: string
  mime: string
  width: number
  height: number
  bytes: number
}

/** attachmentReference is the URL that travels in a message. It is relative on purpose: the server expands
 *  it, and a same-origin relative URL cannot leak a deployment's hostname into the transcript. */
export function attachmentReference(attachmentId: string): string {
  return `/api/v1/agent/attachments/${encodeURIComponent(attachmentId)}`
}

/** attachmentIdOf reads the id back out of a reference, for the remove path. */
export function attachmentIdOf(reference: string | undefined): string | null {
  if (!reference) return null
  const marker = '/agent/attachments/'
  const index = reference.lastIndexOf(marker)
  if (index < 0) return null
  const id = reference.slice(index + marker.length).split('?')[0]
  return id || null
}

export function createImageAttachmentAdapter(): AttachmentAdapter {
  return {
    accept: IMAGE_ACCEPT,

    // add shows the thumbnail immediately, from the local file, before any upload happens: a 5 MB photo
    // on a campus connection must not look like the composer swallowed it.
    async add({ file }): Promise<PendingAttachment> {
      return {
        id: crypto.randomUUID(),
        type: 'image',
        name: file.name,
        contentType: file.type || 'image/png',
        file,
        status: { type: 'running', reason: 'uploading', progress: 0 },
      }
    },

    // send is where the upload happens, so a message is only ever sent with attachments the server has
    // already accepted and measured.
    async send(attachment): Promise<CompleteAttachment> {
      const form = new FormData()
      form.append('file', attachment.file, attachment.name)
      const { data } = await request<UploadResponse>('/agent/attachments', {
        method: 'POST',
        headers: { 'Idempotency-Key': idem() },
        body: form,
      })
      return {
        ...attachment,
        status: { type: 'complete' },
        contentType: data.mime,
        content: [{ type: 'image', image: attachmentReference(data.attachment_id) }],
      }
    },

    // remove deletes a sent attachment so an abandoned upload does not wait for the TTL sweep (§4.5).
    // A pending one has no server row yet, and a failed delete is not worth blocking the composer on.
    async remove(attachment) {
      const part = (attachment as CompleteAttachment).content?.find(entry => entry.type === 'image')
      const id = attachmentIdOf(part && 'image' in part ? String(part.image) : undefined)
      if (!id) return
      await request(`/agent/attachments/${encodeURIComponent(id)}`, { method: 'DELETE' }).catch(() => undefined)
    },
  }
}
