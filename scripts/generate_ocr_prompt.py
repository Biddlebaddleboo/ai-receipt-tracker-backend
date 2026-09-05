#!/usr/bin/env python3
from __future__ import annotations

import argparse
import hashlib
import struct
from pathlib import Path

from PIL import Image, ImageDraw, ImageFont

WIDTH = 159
HEIGHT = 286
FONT_SIZE = 8
MARGIN = 1
LINE_SPACING = 1
CONTENT_WIDTH = WIDTH - 2 * MARGIN
EXPECTED_FONT_SHA256 = "6249d77fa313215cd66a97b942041ddbca73664309ac5e25bd0ab937d727c96e"

PROMPT = (
    "Extract the readable text from this receipt image and summarize the line items and totals. "
    "After the summary, output a JSON object with the following keys: `vendor`, `subtotal`, `tax`, `total`, "
    "`category`, `purchase_date`, `invoice_id`, and `items`. For `invoice_id`, extract only a clearly labelled "
    "merchant-issued identifier such as Invoice #, Invoice ID, Receipt #, Transaction ID, Transaction #, Order #, "
    "Reference #, Bill #, or an obviously equivalent label. When present, `invoice_id` MUST ALWAYS be a JSON string, "
    "even if the merchant identifier consists entirely of digits (never a JSON number). Preserve every character "
    "exactly as printed, including letters, dashes, and leading zeros. For example, if the receipt shows "
    "`Transaction #: 00123456`, return `{\"invoice_id\": \"00123456\"}`, not `{\"invoice_id\": 123456}`. "
    "Do not invent or infer an identifier, and use null when no clear merchant-issued identifier exists. "
    "The `items` array should include objects with `name`, `quantity`, "
    "and `price`. Ensure the `subtotal` equals the sum of each item's `quantity` multiplied by its `price`; if you "
    "can't confirm a value, set it to null. Do not add any explanation outside the JSON object."
)


def sha256(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as f:
        for chunk in iter(lambda: f.read(1024 * 1024), b""):
            h.update(chunk)
    return h.hexdigest()


def wrap_prompt(font: ImageFont.FreeTypeFont) -> list[str]:
    lines: list[str] = []
    current = ""
    for word in PROMPT.split():
        candidate = word if not current else f"{current} {word}"
        if font.getlength(candidate) <= CONTENT_WIDTH:
            current = candidate
        else:
            if not current:
                raise ValueError(f"single word exceeds content width: {word!r}")
            lines.append(current)
            current = word
    if current:
        lines.append(current)
    return lines


def render(font_path: Path) -> Image.Image:
    actual_hash = sha256(font_path)
    if actual_hash != EXPECTED_FONT_SHA256:
        raise ValueError(
            f"unexpected Fira Sans font SHA-256: {actual_hash}; expected {EXPECTED_FONT_SHA256}"
        )

    font = ImageFont.truetype(str(font_path), FONT_SIZE)
    lines = wrap_prompt(font)
    if len(lines) != 29:
        raise ValueError(f"unexpected wrapped line count: {len(lines)} (expected 29)")

    probe = Image.new("RGB", (1, 1), "white")
    probe_draw = ImageDraw.Draw(probe)
    metrics: list[tuple[str, tuple[int, int, int, int]]] = []
    required_height = MARGIN * 2

    for index, line in enumerate(lines):
        bbox = probe_draw.textbbox((0, 0), line, font=font)
        metrics.append((line, bbox))
        required_height += bbox[3] - bbox[1]
        if index != len(lines) - 1:
            required_height += LINE_SPACING

    if required_height != HEIGHT:
        raise ValueError(
            f"rendered height would be {required_height}px; expected exactly {HEIGHT}px"
        )

    image = Image.new("RGB", (WIDTH, HEIGHT), "white")
    draw = ImageDraw.Draw(image)
    y = MARGIN
    for line, bbox in metrics:
        left, top, right, bottom = bbox
        draw.text((MARGIN - left, y - top), line, font=font, fill="black")
        y += (bottom - top) + LINE_SPACING

    if y - LINE_SPACING + MARGIN != HEIGHT:
        raise ValueError("final text placement did not end at the expected bottom margin")
    return image


def verify_webp(path: Path) -> None:
    data = path.read_bytes()
    if len(data) < 12 or data[:4] != b"RIFF" or data[8:12] != b"WEBP":
        raise ValueError("generated asset is not a RIFF/WEBP file")

    declared = struct.unpack("<I", data[4:8])[0] + 8
    if declared != len(data):
        raise ValueError(f"WebP is truncated: header={declared} actual={len(data)}")

    with Image.open(path) as decoded:
        decoded.load()
        if decoded.size != (WIDTH, HEIGHT):
            raise ValueError(f"unexpected decoded dimensions: {decoded.size}")
        if decoded.mode != "RGB":
            decoded = decoded.convert("RGB")


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--font", type=Path, required=True)
    parser.add_argument(
        "--output",
        type=Path,
        default=Path("cmd/apiserver/ocr_prompt.webp"),
    )
    args = parser.parse_args()

    image = render(args.font)
    args.output.parent.mkdir(parents=True, exist_ok=True)
    image.save(args.output, format="WEBP", lossless=True, quality=100, method=6, exact=True)
    verify_webp(args.output)

    print(f"generated {args.output}")
    print(f"dimensions={WIDTH}x{HEIGHT}")
    print(f"bytes={args.output.stat().st_size}")
    print(f"sha256={sha256(args.output)}")


if __name__ == "__main__":
    main()
