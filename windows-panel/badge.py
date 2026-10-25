"""800 x 480 badge layout based on myled's output and badge.py template."""
from __future__ import annotations

import base64
import io
from datetime import datetime, timedelta, timezone
from pathlib import Path

from PIL import Image, ImageDraw, ImageFont, ImageOps
from wolink.image import quantize_to_palette

SHANGHAI = timezone(timedelta(hours=8), "Asia/Shanghai")
WHITE, BLACK, RED = "white", "black", (255, 0, 0)


def font(size, bold=False):
    for filename in ("msyhbd.ttc" if bold else "msyh.ttc", "simhei.ttf"):
        path = Path("C:/Windows/Fonts") / filename
        if path.exists():
            return ImageFont.truetype(str(path), size)
    return ImageFont.load_default(size=size)


def wrap(draw, text, width, face):
    result = []
    for paragraph in str(text).replace("\r", "").split("\n"):
        line = ""
        for char in paragraph:
            if line and draw.textlength(line + char, font=face) > width:
                result.append(line)
                line = char
            else:
                line += char
        result.append(line)
    return result or [""]


def fit_text(draw, position, text, width, size=23, fill=BLACK, bold=False):
    # Profile fields are bounded by the backend. Wrap instead of silently dropping text.
    while size > 10 and draw.textlength(str(text), font=font(size, bold)) > width:
        size -= 1
    draw.text(position, str(text), font=font(size, bold), fill=fill)


def task_rows(tasks, draw):
    rows = []
    for number, task in enumerate(tasks, 1):
        title = task.get("title") or "未命名任务"
        if task.get("detail"):
            title += "\n" + task["detail"]
        size = 21
        lines = wrap(draw, title, 480, font(size))
        while len(lines) > 2 and size > 14:
            size -= 1
            lines = wrap(draw, title, 480, font(size))
        status = {"satisfied": "已完成", "not_completed": "未完成", "skipped": "已跳过"}.get(task.get("status"))
        if not status:
            status = "进行中" if task.get("task_status") == "in_progress" or task.get("status") == "in_progress" else "待完成"
        for start in range(0, len(lines), 2):
            rows.append({"number": f"{number}{'续' if start else ''}", "lines": lines[start:start + 2], "size": size, "status": status})
    return rows


def render_snapshot(snapshot, page=0):
    profile = snapshot.get("profile", {})
    task_data = snapshot.get("tasks", {})
    overrides = profile.get("task_overrides") or {}
    tasks = []
    for source in (task_data.get("today_items") or []):
        if source.get("status") == "superseded":
            continue
        custom = overrides.get(source.get("logical_id") or source.get("id"), {})
        item = dict(source)
        item["detail"] = ""
        # Include every current item in FastTask's order. Legacy visibility and
        # ordering preferences cannot hide tasks or their completion state.
        for key in ("title", "detail"):
            if key in custom:
                item[key] = custom[key]
        item["title"] = (item.get("title") or "").strip() or source.get("title") or "未命名任务"
        item["detail"] = (item.get("detail") or "").strip()
        tasks.append(item)
    tasks.sort(key=lambda item: item.get("position", 0))
    image = Image.new("RGB", (800, 480), WHITE)
    draw = ImageDraw.Draw(image)
    rows = task_rows(tasks, draw)
    pages = max(1, (len(rows) + 2) // 3)
    # A saved page that no longer exists resets visibly to page one, rather than showing empty data.
    page = page if 0 <= page < pages else 0
    draw.rectangle((0, 0, 799, 51), fill=RED)
    fit_text(draw, (24, 6), profile.get("title") or "今日任务", 590, 30, WHITE, True)
    draw.text((677, 14), "FastTask", font=font(19, True), fill=WHITE)

    portrait = Image.new("RGB", (176, 174), WHITE)
    avatar = profile.get("avatar_data_url") or ""
    if avatar:
        try:
            encoded = avatar.split(",", 1)[1]
            data = base64.b64decode(encoded, validate=True)
            if len(data) > 1024 * 1024:
                raise ValueError("avatar too large")
            with Image.open(io.BytesIO(data)) as source:
                if source.width * source.height > 4_000_000:
                    raise ValueError("avatar dimensions too large")
                rgba = ImageOps.exif_transpose(source).convert("RGBA")
                white = Image.new("RGBA", rgba.size, WHITE)
                white.alpha_composite(rgba)
                fitted = ImageOps.contain(white.convert("RGB"), (176, 174), Image.Resampling.LANCZOS)
                portrait.paste(fitted, ((176 - fitted.width) // 2, (174 - fitted.height) // 2))
                portrait = quantize_to_palette(portrait, dither=True)
        except (ValueError, OSError, IndexError):
            ImageDraw.Draw(portrait).text((27, 67), "照片不可用", font=font(20), fill=BLACK)
    else:
        pd = ImageDraw.Draw(portrait)
        pd.rectangle((0, 0, 176, 174), fill=RED)
        pd.ellipse((58, 22, 118, 82), fill=WHITE)
        pd.rounded_rectangle((28, 97, 148, 174), radius=35, fill=WHITE)
    image.paste(portrait, (24, 70))
    draw.rectangle((23, 69, 200, 244), outline=BLACK, width=3)
    fit_text(draw, (229, 70), profile.get("display_name") or "未设置姓名", 545, 42, BLACK, True)
    draw.line((230, 126, 775, 126), fill=RED, width=3)
    weather = snapshot.get("weather", {})
    local_date = task_data.get("local_date") or datetime.now(SHANGHAI).date().isoformat()
    manual_weather = (profile.get("weather_override") or "").strip()
    weather_current = weather.get("status") in ("ok", "stale") and weather.get("local_date") == local_date
    weather_text = manual_weather or ("东西湖 · " + (weather.get("summary") if weather_current and weather.get("summary") else "天气暂不可用"))
    if not manual_weather and weather_current and weather.get("status") == "stale":
        weather_text += "（缓存）"
    elif manual_weather:
        weather_text += "（手动）"
    draw.text((230, 157), "今日天气", font=font(22, True), fill=RED)
    fit_text(draw, (340, 158), weather_text, 435, 21)
    day = datetime.strptime(local_date, "%Y-%m-%d")
    date_text = f"{day:%Y.%m.%d}  周{'一二三四五六日'[day.weekday()]}"
    draw.text((230, 205), "今日日期", font=font(18, True), fill=RED)
    draw.text((340, 205), date_text, font=font(18), fill=BLACK)

    draw.rectangle((24, 264, 776, 457), outline=BLACK, width=2)
    draw.rectangle((24, 264, 776, 307), fill=RED)
    for x in (112, 628):
        draw.line((x, 264, x, 457), fill=BLACK, width=2)
    for y in (357, 407):
        draw.line((24, y, 776, y), fill=BLACK, width=2)
    draw.text((45, 272), "序号", font=font(21, True), fill=WHITE)
    draw.text((317, 272), "今日任务", font=font(21, True), fill=WHITE)
    draw.text((657, 272), "完成状态", font=font(21, True), fill=WHITE)
    for index, row in enumerate(rows[page * 3:page * 3 + 3]):
        top = 307 + index * 50
        draw.text((45, top + 10), row["number"], font=font(19), fill=BLACK)
        for line_index, line in enumerate(row["lines"]):
            y = top + (4 if len(row["lines"]) > 1 else 10) + line_index * (row["size"] + 1)
            draw.text((130, y), line, font=font(row["size"]), fill=BLACK)
        draw.text((660, top + 11), row["status"], font=font(20, True), fill=RED if row["status"] == "已完成" else BLACK)
    if not rows:
        draw.text((130, 319), "今日暂无任务", font=font(21), fill=BLACK)
    footer = f"第 {page + 1}/{pages} 页"
    draw.text((776-draw.textlength(footer,font=font(12)), 461), footer, font=font(12), fill=BLACK)
    return quantize_to_palette(image), {"page": page, "pages": pages, "task_count": len(tasks), "local_date": local_date}
