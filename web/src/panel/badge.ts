import { DISPLAY_HEIGHT, DISPLAY_WIDTH } from './protocol'

export type TaskOverride = { title?: string; detail?: string; selected?: boolean; position?: number }
export type PanelProfile = { user_id?: string; display_name: string; seat: string; title: string; avatar_data_url: string; weather_override: string; task_overrides: Record<string, TaskOverride>; revision?: number; updated_at?: string }
export type PanelWeather = { status: 'ok' | 'stale' | 'unavailable'; location: string; local_date: string; summary: string; source: string; source_url?: string; observed_at?: string; fetched_at?: string; message?: string }
export type BadgeTask = { key: string; title: string; status: string }
export type BadgeRow = { task: BadgeTask; number: number; continuation: boolean; lines: string[]; fontSize: number }
export type BadgePage = { rows: BadgeRow[] }
export const EMPTY_PROFILE: PanelProfile = { display_name: '', seat: '', title: '今日任务', avatar_data_url: '', weather_override: '', task_overrides: {} }
const FONT = '"Microsoft YaHei", "Segoe UI", sans-serif'

export function wrapBadgeText(context: CanvasRenderingContext2D, text: string, width: number) {
  const lines: string[] = []
  for (const paragraph of text.replace(/\r\n?/g, '\n').split('\n')) {
    let line = ''
    for (const character of Array.from(paragraph)) {
      if (line && context.measureText(line + character).width > width) { lines.push(line); line = character }
      else line += character
    }
    lines.push(line)
  }
  return lines
}

export function paginateBadge(tasks: BadgeTask[]): BadgePage[] {
  const context = document.createElement('canvas').getContext('2d')
  if (!context) return []
  const rows: BadgeRow[] = []
  tasks.forEach((task, index) => {
    let size = 21, lines: string[] = []
    do { context.font = `${size}px ${FONT}`; lines = wrapBadgeText(context, task.title, 480); if (lines.length <= 2 || size === 14) break; size-- } while (size >= 14)
    // Extremely long titles continue in subsequent rows/pages, preserving every
    // character. Each physical screen always keeps the template's three rows.
    for (let line = 0; line < lines.length; line += 2) rows.push({ task, number: index + 1, continuation: line > 0, lines: lines.slice(line, line + 2), fontSize: size })
  })
  const pages: BadgePage[] = []
  for (let index = 0; index < rows.length; index += 3) pages.push({ rows: rows.slice(index, index + 3) })
  return pages.length ? pages : [{ rows: [] }]
}

function fit(context: CanvasRenderingContext2D, value: string, width: number, size: number, bold = false) {
  do { context.font = `${bold ? '700 ' : ''}${size}px ${FONT}`; if (context.measureText(value).width <= width) break; size-- } while (size > 7)
}

export function quantizeBadge(image: ImageData, dither = false) {
  const { data, width, height } = image
  const values = dither ? new Float32Array(data) : null
  for (let y = 0; y < height; y++) for (let x = 0; x < width; x++) {
    const offset = (y * width + x) * 4
    const r = values ? values[offset] : data[offset], g = values ? values[offset + 1] : data[offset + 1], b = values ? values[offset + 2] : data[offset + 2]
    const red = r > 100 && r > g * 1.4 && r > b * 1.4
    const white = !red && (r + g + b) / 3 >= (dither ? 128 : 170)
    const next = [red || white ? 255 : 0, white ? 255 : 0, white ? 255 : 0]
    for (let channel = 0; channel < 3; channel++) {
      data[offset + channel] = next[channel]
      if (values) {
        const error = values[offset + channel] - next[channel]
        for (const [dx, dy, weight] of [[1, 0, 7 / 16], [-1, 1, 3 / 16], [0, 1, 5 / 16], [1, 1, 1 / 16]]) {
          if (x + dx >= 0 && x + dx < width && y + dy < height) values[((y + dy) * width + x + dx) * 4 + channel] += error * weight
        }
      }
    }
    data[offset + 3] = 255
  }
  return image
}

export function drawBadge(canvas: HTMLCanvasElement, page: BadgePage | undefined, profile: PanelProfile, weather: string, date: string, pageIndex: number, pageCount: number, avatar: HTMLImageElement | null) {
  canvas.width = DISPLAY_WIDTH; canvas.height = DISPLAY_HEIGHT
  const context = canvas.getContext('2d', { willReadFrequently: true })
  if (!context) throw new Error('浏览器无法绘制工牌，请使用 Windows Edge 或 Chrome')
  context.fillStyle = '#fff'; context.fillRect(0, 0, 800, 480); context.textBaseline = 'top'
  context.fillStyle = '#f00'; context.fillRect(0, 0, 800, 52)
  context.fillStyle = '#fff'; fit(context, profile.title.trim() || '今日任务', 620, 30, true); context.fillText(profile.title.trim() || '今日任务', 24, 9)
  context.font = `700 19px ${FONT}`; context.textAlign = 'right'; context.fillText('FastTask', 776, 17); context.textAlign = 'left'
  if (avatar) {
    const portrait = document.createElement('canvas'); portrait.width = 176; portrait.height = 174
    const photo = portrait.getContext('2d', { willReadFrequently: true })!
    photo.fillStyle = '#fff'; photo.fillRect(0, 0, 176, 174)
    const scale = Math.min(176 / avatar.naturalWidth, 174 / avatar.naturalHeight)
    const width = avatar.naturalWidth * scale, height = avatar.naturalHeight * scale
    photo.drawImage(avatar, (176 - width) / 2, (174 - height) / 2, width, height)
    photo.putImageData(quantizeBadge(photo.getImageData(0, 0, 176, 174), true), 0, 0)
    context.drawImage(portrait, 24, 70)
  } else {
    context.fillStyle = '#f00'; context.fillRect(24, 70, 176, 174)
    context.fillStyle = '#fff'; context.beginPath(); context.arc(112, 122, 30, 0, Math.PI * 2); context.fill()
    context.beginPath(); context.ellipse(112, 225, 60, 58, 0, Math.PI, 0); context.fill()
    context.fillRect(52, 219, 120, 25)
  }
  context.strokeStyle = '#000'; context.lineWidth = 2; context.strokeRect(24, 70, 176, 174)
  context.fillStyle = '#000'; fit(context, profile.display_name.trim() || '未设置姓名', 546, 42, true); context.fillText(profile.display_name.trim() || '未设置姓名', 230, 75)
  context.fillStyle = '#f00'; context.fillRect(230, 126, 546, 3)
  context.font = `700 22px ${FONT}`; context.fillText('今日天气', 230, 157)
  context.fillStyle = '#000'; fit(context, weather || '天气暂不可用', 435, 21); context.fillText(weather || '天气暂不可用', 340, 158)
  context.fillStyle = '#f00'; context.font = `700 20px ${FONT}`; context.fillText('今日日期', 230, 205)
  const weekday = '日一二三四五六'[new Date(`${date}T12:00:00`).getDay()]
  context.fillStyle = '#000'; context.font = `20px ${FONT}`; context.fillText(`${date.replaceAll('-', '.')}  周${weekday}`, 340, 205)
  const top = 264, headerBottom = 307
  context.fillStyle = '#f00'; context.fillRect(24, top, 752, 43)
  context.strokeStyle = '#000'; context.lineWidth = 2; context.strokeRect(24, top, 752, 193)
  for (const x of [112, 628]) { context.beginPath(); context.moveTo(x, top); context.lineTo(x, 457); context.stroke() }
  for (const y of [307, 357, 407]) { context.beginPath(); context.moveTo(24, y); context.lineTo(776, y); context.stroke() }
  context.fillStyle = '#fff'; context.font = `700 21px ${FONT}`
  context.fillText('序号', 45, 275); context.fillText('今日任务', 317, 275); context.fillText('完成状态', 659, 275)
  page?.rows.forEach((row, index) => {
    const y = headerBottom + index * 50
    context.fillStyle = '#000'; context.font = `${row.continuation ? 16 : 21}px ${FONT}`; context.textAlign = 'center'; context.fillText(`${row.number}${row.continuation ? '续' : ''}`, 68, y + 15); context.textAlign = 'left'
    context.font = `${row.fontSize}px ${FONT}`
    const height = row.lines.length * (row.fontSize + 2)
    row.lines.forEach((line, lineIndex) => context.fillText(line, 130, y + (50 - height) / 2 + lineIndex * (row.fontSize + 2)))
    context.fillStyle = row.task.status === '待完成' ? '#000' : '#f00'; context.font = `700 21px ${FONT}`; context.textAlign = 'center'; context.fillText(row.task.status, 702, y + 14); context.textAlign = 'left'
  })
  if (!page?.rows.length) { context.fillStyle = '#000'; context.font = `21px ${FONT}`; context.fillText('今日暂无任务', 130, 322) }
  context.fillStyle = '#000'; context.font = `12px ${FONT}`
  context.textAlign = 'right'; context.fillText(`第 ${pageCount ? pageIndex + 1 : 0} / ${pageCount} 页`, 776, 463); context.textAlign = 'left'
  const image = quantizeBadge(context.getImageData(0, 0, 800, 480))
  context.putImageData(image, 0, 0)
  return image
}

export async function prepareAvatar(file: File): Promise<string> {
  if (!['image/png', 'image/jpeg', 'image/webp'].includes(file.type)) throw new Error('请上传 PNG、JPEG 或 WebP 头像')
  if (file.size > 10 * 1024 * 1024) throw new Error('头像文件不能超过 10 MB')
  const bitmap = await createImageBitmap(file, { imageOrientation: 'from-image' })
  try {
    const scale = Math.min(1, 512 / Math.max(bitmap.width, bitmap.height))
    const canvas = document.createElement('canvas'); canvas.width = Math.max(1, Math.round(bitmap.width * scale)); canvas.height = Math.max(1, Math.round(bitmap.height * scale))
    const context = canvas.getContext('2d')!
    context.fillStyle = '#fff'; context.fillRect(0, 0, canvas.width, canvas.height); context.drawImage(bitmap, 0, 0, canvas.width, canvas.height)
    return canvas.toDataURL('image/jpeg', .9)
  } finally { bitmap.close() }
}
