import type { PendingAttachment } from '@assistant-ui/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { request } from '../api'
import { attachmentIdOf, attachmentReference, createImageAttachmentAdapter, IMAGE_ACCEPT } from './attachments'

// The image attachment adapter (doc/chat-features.md §4.2).
//
// The rule under test is the one that keeps SQLite from filling up with photographs: a message carries a
// reference to a server-owned file, never the bytes. SimpleImageAttachmentAdapter would inline a data URI,
// which is exactly what §4.2 rejects.

vi.mock('../api', () => ({
  request: vi.fn(),
  idem: vi.fn(() => 'idem-attachment-1'),
  ensureFreshAccessToken: vi.fn(async () => 'jwt'),
}))

const mockRequest = vi.mocked(request)

function fileOf(name = 'plot.png', type = 'image/png'): File {
  return new File([new Uint8Array([0x89, 0x50, 0x4e, 0x47])], name, { type })
}

// add() may return a generator for a progressive upload. This adapter never does, and narrowing here keeps
// the assertions pointed at the value the composer actually receives.
async function asPending(added: PendingAttachment | AsyncGenerator<PendingAttachment, void> | Promise<PendingAttachment>): Promise<PendingAttachment> {
  const value = await added
  if (typeof (value as AsyncGenerator<PendingAttachment, void>).next === 'function') {
    throw new Error('the adapter returned a generator; these tests expect one pending attachment')
  }
  return value as PendingAttachment
}

beforeEach(() => {
  mockRequest.mockReset()
})

describe('image attachment adapter', () => {
  it('accepts exactly the whitelist the server enforces (§4.5)', () => {
    expect(IMAGE_ACCEPT).toBe('image/png,image/jpeg,image/webp,image/gif')
    expect(createImageAttachmentAdapter().accept).toBe(IMAGE_ACCEPT)
  })

  it('shows the file immediately and uploads only on send', async () => {
    const adapter = createImageAttachmentAdapter()
    const file = fileOf()
    const pending = await asPending(adapter.add({ file }))
    // No upload yet: a 5 MB photo on a campus connection must not block the composer.
    expect(mockRequest).not.toHaveBeenCalled()
    expect(pending.status).toMatchObject({ type: 'running', reason: 'uploading' })
    expect(pending.file).toBe(file)
    expect(pending.type).toBe('image')

    mockRequest.mockResolvedValueOnce({
      data: { attachment_id: 'att_1', mime: 'image/png', width: 4, height: 3, bytes: 4 },
      etag: null,
    } as never)
    const sent = await adapter.send(pending as never)
    const [path, init] = mockRequest.mock.calls[0]
    expect(path).toBe('/agent/attachments')
    expect((init as RequestInit).method).toBe('POST')
    // FormData, not a JSON envelope: the server reads a file part and sniffs the bytes.
    expect((init as RequestInit).body).toBeInstanceOf(FormData)
    expect(((init as RequestInit).body as FormData).get('file')).toBeInstanceOf(File)
    expect(sent.status).toEqual({ type: 'complete' })
    // The message carries a reference, and only a reference.
    expect(sent.content).toEqual([{ type: 'image', image: '/api/v1/agent/attachments/att_1' }])
    expect(JSON.stringify(sent.content)).not.toContain('base64')
  })

  it('deletes a sent attachment on remove, and does nothing for an unsent one (§4.5)', async () => {
    const adapter = createImageAttachmentAdapter()
    mockRequest.mockResolvedValue({ data: {}, etag: null } as never)

    await adapter.remove({
      id: 'a1', type: 'image', name: 'plot.png', status: { type: 'complete' },
      content: [{ type: 'image', image: attachmentReference('att_9') }],
    } as never)
    expect(mockRequest.mock.calls[0][0]).toBe('/agent/attachments/att_9')
    expect((mockRequest.mock.calls[0][1] as RequestInit).method).toBe('DELETE')

    mockRequest.mockClear()
    await adapter.remove({ id: 'a2', type: 'image', name: 'x.png', status: { type: 'running', reason: 'uploading', progress: 0 }, file: fileOf() } as never)
    expect(mockRequest).not.toHaveBeenCalled()
  })

  it('reads an id back out of a reference and refuses anything else', () => {
    expect(attachmentIdOf('/api/v1/agent/attachments/att_1')).toBe('att_1')
    expect(attachmentIdOf('https://fasttask.example/api/v1/agent/attachments/att_2?x=1')).toBe('att_2')
    expect(attachmentIdOf('data:image/png;base64,AAAA')).toBeNull()
    expect(attachmentIdOf(undefined)).toBeNull()
    expect(attachmentIdOf('/api/v1/agent/attachments/')).toBeNull()
  })

  it('does not swallow a failed upload: the message must not send without its image', async () => {
    const adapter = createImageAttachmentAdapter()
    mockRequest.mockRejectedValueOnce(new Error('ATTACHMENT_TOO_LARGE: the image is larger than the 8 MB limit'))
    const pending = await asPending(adapter.add({ file: fileOf('huge.png') }))
    await expect(adapter.send(pending as never)).rejects.toThrow(/ATTACHMENT_TOO_LARGE/)
  })
})
