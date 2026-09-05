#!/usr/bin/env python3
import argparse
import struct
from pathlib import Path
from PIL import Image, ImageDraw, ImageFont

WIDTH=159
FONT_SIZE=8
MARGIN=1
LINE_SPACING=1
PROMPT='Extract the readable text from this receipt image and line items/totals. Output only this positional JSON array: [vendor,subtotal,tax,total,category,purchase_date,invoice_id,items], where items=[[name,quantity,price],...]. purchase_date: extract the receipt purchase/transaction date as printed; use null if no clear date. invoice_id: only a clearly labelled merchant-issued ID such as Invoice #, Invoice ID, Receipt #, Transaction ID, Transaction #, Order #, Reference #, Bill #, or obvious equivalent. If present, invoice_id MUST ALWAYS be a JSON string even if all digits; preserve exact characters including letters, dashes, and leading zeros. If `Transaction #: 00123456`, the seventh value must be `"00123456"`, not `123456`. Never invent or infer an ID; use null when no clear merchant-issued identifier exists. subtotal must equal sum(quantity*price). Unknown scalar=null; no confirmable items=[]. JSON only.'

def wrap(draw,font,text,max_width):
    lines=[]; current=""
    for word in text.split():
        candidate=word if not current else current+" "+word
        if not current or draw.textlength(candidate,font=font)<=max_width:
            current=candidate
        else:
            lines.append(current); current=word
    if current: lines.append(current)
    return lines

def render(font_path):
    font=ImageFont.truetype(str(font_path),FONT_SIZE)
    probe=Image.new("RGB",(1,1),"white"); draw=ImageDraw.Draw(probe)
    lines=wrap(draw,font,PROMPT,WIDTH-2*MARGIN)
    metrics=[]; height=2*MARGIN
    for i,line in enumerate(lines):
        bbox=draw.textbbox((0,0),line,font=font); metrics.append((line,bbox)); height+=bbox[3]-bbox[1]
        if i!=len(lines)-1: height+=LINE_SPACING
    image=Image.new("RGB",(WIDTH,height),"white"); out=ImageDraw.Draw(image); y=MARGIN
    for line,bbox in metrics:
        l,t,r,b=bbox; out.text((MARGIN-l,y-t),line,font=font,fill="black"); y+=(b-t)+LINE_SPACING
    return image,len(lines)

def verify(path):
    data=path.read_bytes()
    if data[:4]!=b"RIFF" or data[8:12]!=b"WEBP": raise SystemExit("invalid WebP")
    if struct.unpack("<I",data[4:8])[0]+8!=len(data): raise SystemExit("truncated WebP")

def main():
    ap=argparse.ArgumentParser(); ap.add_argument("--font",required=True); ap.add_argument("--png",required=True); ap.add_argument("--webp",required=True); a=ap.parse_args()
    image,lines=render(Path(a.font)); png=Path(a.png); webp=Path(a.webp); png.parent.mkdir(parents=True,exist_ok=True)
    image.save(png,format="PNG",optimize=True); image.save(webp,format="WEBP",lossless=True,quality=100,method=6,exact=True); verify(webp)
    patches=((image.width+31)//32)*((image.height+31)//32)
    print(f"Positional prompt: {image.width}x{image.height}, lines={lines}, patches={patches}, png={png.stat().st_size}, webp={webp.stat().st_size}")

if __name__=="__main__": main()
