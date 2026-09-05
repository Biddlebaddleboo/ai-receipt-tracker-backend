#!/usr/bin/env python3
import argparse
import struct
from pathlib import Path
from PIL import Image, ImageDraw, ImageFont

ROOT = Path(__file__).resolve().parents[1]
APISERVER = ROOT / "cmd" / "apiserver"
PROMPT = (
    "Extract readable receipt text and line items/totals. Output only this positional JSON array: "
    "[vendor,subtotal,tax,total,category,purchase_date,invoice_id,items], where items=[[name,quantity,price],...]. "
    "invoice_id: only a clearly labelled merchant-issued ID such as Invoice #, Invoice ID, Receipt #, Transaction ID, Transaction #, Order #, Reference #, Bill #, or obvious equivalent. "
    "If present, invoice_id MUST ALWAYS be a JSON string even if all digits; preserve exact characters including letters, dashes, and leading zeros. "
    "If `Transaction #: 00123456`, the seventh value must be `\"00123456\"`, not `123456`. Never invent or infer an ID; use null when no clear merchant-issued identifier exists. "
    "subtotal must equal sum(quantity*price). Unknown scalar=null; no confirmable items=[]. JSON only."
)

GO_HELPER = r'''package main

import (
    "encoding/json"
    "strings"
)

const (
    ocrPosVendor = iota
    ocrPosSubtotal
    ocrPosTax
    ocrPosTotal
    ocrPosCategory
    ocrPosPurchaseDate
    ocrPosInvoiceID
    ocrPosItems
)

func extractPositionalJSON(rawText string) ([]interface{}, bool) {
    trimmed := strings.TrimSpace(rawText)
    if trimmed == "" {
        return nil, false
    }
    var payload []interface{}
    if err := json.Unmarshal([]byte(trimmed), &payload); err == nil {
        return payload, true
    }
    start := strings.Index(trimmed, "[")
    end := strings.LastIndex(trimmed, "]")
    if start < 0 || end <= start {
        return nil, false
    }
    if err := json.Unmarshal([]byte(trimmed[start:end+1]), &payload); err != nil {
        return nil, false
    }
    return payload, true
}

func readPositionalStructuredFields(rawText string, payload []interface{}, categoryOptions []string) ocrResult {
    at := func(index int) interface{} {
        if index < 0 || index >= len(payload) {
            return nil
        }
        return payload[index]
    }
    category := normalizeString(at(ocrPosCategory))
    if len(categoryOptions) > 0 {
        category = validateReceiptCategory(category, categoryOptions)
    }
    return ocrResult{
        Text: rawText,
        Vendor: normalizeString(at(ocrPosVendor)),
        Subtotal: normalizeAmount(at(ocrPosSubtotal)),
        Tax: normalizeAmount(at(ocrPosTax)),
        Total: normalizeAmount(at(ocrPosTotal)),
        Category: category,
        PurchaseDate: normalizeString(at(ocrPosPurchaseDate)),
        InvoiceID: normalizeMerchantIdentifier(at(ocrPosInvoiceID)),
        Items: extractPositionalReceiptItems(at(ocrPosItems)),
    }
}

func extractPositionalReceiptItems(raw interface{}) []ocrItem {
    entries, ok := raw.([]interface{})
    if !ok {
        return nil
    }
    items := make([]ocrItem, 0, len(entries))
    for _, rawEntry := range entries {
        entry, ok := rawEntry.([]interface{})
        if !ok {
            continue
        }
        var nameRaw, quantityRaw, priceRaw interface{}
        if len(entry) > 0 { nameRaw = entry[0] }
        if len(entry) > 1 { quantityRaw = entry[1] }
        if len(entry) > 2 { priceRaw = entry[2] }
        name := normalizeString(nameRaw)
        quantity := normalizeAmount(quantityRaw)
        price := normalizeAmount(priceRaw)
        if name == nil && quantity == nil && price == nil {
            continue
        }
        items = append(items, ocrItem{Name: name, Quantity: quantity, Price: price})
    }
    return items
}
'''

GO_TEST = r'''package main

import "testing"

func TestReadStructuredFieldsPositionalJSON(t *testing.T) {
    raw := `["Costco",18.99,2.47,21.46,"Meals","2026-09-05","00123456",[["Pizza",1,18.99]]]`
    got := readStructuredFields(raw, []string{"Meals"})
    if got.Vendor == nil || *got.Vendor != "Costco" { t.Fatalf("vendor=%v", got.Vendor) }
    if got.Subtotal == nil || *got.Subtotal != 18.99 { t.Fatalf("subtotal=%v", got.Subtotal) }
    if got.Tax == nil || *got.Tax != 2.47 { t.Fatalf("tax=%v", got.Tax) }
    if got.Total == nil || *got.Total != 21.46 { t.Fatalf("total=%v", got.Total) }
    if got.Category == nil || *got.Category != "Meals" { t.Fatalf("category=%v", got.Category) }
    if got.PurchaseDate == nil || *got.PurchaseDate != "2026-09-05" { t.Fatalf("purchase_date=%v", got.PurchaseDate) }
    if got.InvoiceID == nil || *got.InvoiceID != "00123456" { t.Fatalf("invoice_id=%v", got.InvoiceID) }
    if len(got.Items) != 1 { t.Fatalf("items=%#v", got.Items) }
    if got.Items[0].Name == nil || *got.Items[0].Name != "Pizza" { t.Fatalf("item.name=%v", got.Items[0].Name) }
    if got.Items[0].Quantity == nil || *got.Items[0].Quantity != 1 { t.Fatalf("item.quantity=%v", got.Items[0].Quantity) }
    if got.Items[0].Price == nil || *got.Items[0].Price != 18.99 { t.Fatalf("item.price=%v", got.Items[0].Price) }
}

func TestReadStructuredFieldsObjectFallback(t *testing.T) {
    got := readStructuredFields(`{"vendor":"Costco","invoice_id":"00123456"}`, nil)
    if got.Vendor == nil || *got.Vendor != "Costco" { t.Fatalf("vendor=%v", got.Vendor) }
    if got.InvoiceID == nil || *got.InvoiceID != "00123456" { t.Fatalf("invoice_id=%v", got.InvoiceID) }
}

func TestPositionalInvoiceIDLeadingZeros(t *testing.T) {
    got := readStructuredFields(`[null,null,null,null,null,null,"00123456",[]]`, nil)
    if got.InvoiceID == nil || *got.InvoiceID != "00123456" { t.Fatalf("invoice_id=%v", got.InvoiceID) }
}
'''

GENERATOR = '''#!/usr/bin/env python3
import argparse
import struct
from pathlib import Path
from PIL import Image, ImageDraw, ImageFont

WIDTH=159
FONT_SIZE=8
MARGIN=1
LINE_SPACING=1
PROMPT={prompt!r}

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
    print(f"Positional prompt: {{image.width}}x{{image.height}}, lines={{lines}}, patches={{patches}}, png={{png.stat().st_size}}, webp={{webp.stat().st_size}}")

if __name__=="__main__": main()
'''.format(prompt=PROMPT)

def replace_function(text, name, replacement, next_name):
    start=text.index(f"func {name}")
    end=text.index(f"\nfunc {next_name}", start)
    return text[:start]+replacement+text[end:]

def patch_text(path, old, new, label):
    s=path.read_text()
    if old not in s:
        raise SystemExit(f"{label} anchor not found")
    path.write_text(s.replace(old,new,1))

def render_assets(font_path):
    font=ImageFont.truetype(str(font_path),8)
    probe=Image.new("RGB",(1,1),"white"); draw=ImageDraw.Draw(probe)
    lines=[]; current=""; max_width=157
    for word in PROMPT.split():
        candidate=word if not current else current+" "+word
        if not current or draw.textlength(candidate,font=font)<=max_width: current=candidate
        else: lines.append(current); current=word
    if current: lines.append(current)
    metrics=[]; height=2
    for i,line in enumerate(lines):
        bbox=draw.textbbox((0,0),line,font=font); metrics.append((line,bbox)); height+=bbox[3]-bbox[1]
        if i!=len(lines)-1: height+=1
    image=Image.new("RGB",(159,height),"white"); out=ImageDraw.Draw(image); y=1
    for line,bbox in metrics:
        l,t,r,b=bbox; out.text((1-l,y-t),line,font=font,fill="black"); y+=(b-t)+1
    png=APISERVER/"ocr_prompt_array.png"; webp=APISERVER/"ocr_prompt_array.webp"
    image.save(png,format="PNG",optimize=True); image.save(webp,format="WEBP",lossless=True,quality=100,method=6,exact=True)
    data=webp.read_bytes()
    if data[:4]!=b"RIFF" or data[8:12]!=b"WEBP" or struct.unpack("<I",data[4:8])[0]+8!=len(data): raise SystemExit("invalid generated WebP")
    patches=((159+31)//32)*((height+31)//32)
    print(f"Positional prompt: 159x{height}, lines={len(lines)}, patches={patches}, png={png.stat().st_size}, webp={webp.stat().st_size}")

def main():
    ap=argparse.ArgumentParser(); ap.add_argument("--font",required=True); args=ap.parse_args(); font=Path(args.font)
    (APISERVER/"ocr_positional.go").write_text(GO_HELPER)
    (APISERVER/"ocr_positional_test.go").write_text(GO_TEST)
    receipts=APISERVER/"receipts.go"; s=receipts.read_text()
    old='func readStructuredFields(rawText string, categoryOptions []string) ocrResult {\n\tpayload := extractJSON(rawText)'
    new='func readStructuredFields(rawText string, categoryOptions []string) ocrResult {\n\tif payload, ok := extractPositionalJSON(rawText); ok {\n\t\treturn readPositionalStructuredFields(rawText, payload, categoryOptions)\n\t}\n\tpayload := extractJSON(rawText)'
    if old not in s: raise SystemExit("readStructuredFields anchor not found")
    s=s.replace(old,new,1)
    prompt_func='''func buildOCRPrompt(categoryOptions []string) string {\n\tprompt := "Extract readable receipt text and line items/totals. Output only this positional JSON array: " +\n\t\t"[vendor,subtotal,tax,total,category,purchase_date,invoice_id,items], where items=[[name,quantity,price],...]. " +\n\t\t"invoice_id: only a clearly labelled merchant-issued ID such as Invoice #, Invoice ID, Receipt #, Transaction ID, Transaction #, Order #, Reference #, Bill #, or obvious equivalent. " +\n\t\t"If present, invoice_id MUST ALWAYS be a JSON string even if all digits; preserve exact characters including letters, dashes, and leading zeros. " +\n\t\t"If `Transaction #: 00123456`, the seventh value must be `\\\"00123456\\\"`, not `123456`. Never invent or infer an ID; use null when no clear merchant-issued identifier exists. " +\n\t\t"subtotal must equal sum(quantity*price). Unknown scalar=null; no confirmable items=[]. JSON only."\n\tif len(categoryOptions) == 0 { return prompt }\n\treturn prompt + " Use these categories when guessing the receipt type: " + strings.Join(categoryOptions, ", ") + ". If none match, use null for the category position."\n}\n'''
    s=replace_function(s,"buildOCRPrompt(categoryOptions []string) string {",prompt_func,"collectOCRText(")
    receipts.write_text(s)
    p=APISERVER/"openai_prompt_image_test.go"; s=p.read_text(); s=s.replace('If none match, respond with null for the `category` key.','If none match, use null for the category position.'); p.write_text(s)
    p=APISERVER/"receipt_invoice_test.go"; s=p.read_text(); start=s.index('func TestBuildOCRPromptRequiresStringInvoiceID'); end=s.index('\nfunc TestReceiptInvoiceIDExtractionLeavesMissingOrAmbiguousValuesEmpty',start)
    test_func=r'''func TestBuildOCRPromptRequiresStringInvoiceID(t *testing.T) {
    prompt := buildOCRPrompt(nil)
    for _, phrase := range []string{
        "invoice_id MUST ALWAYS be a JSON string",
        "even if all digits",
        "`Transaction #: 00123456`",
        "seventh value must be `\"00123456\"`",
        "not `123456`",
        "use null when no clear merchant-issued identifier exists",
    } {
        if !strings.Contains(prompt, phrase) { t.Fatalf("prompt does not explicitly require %q: %s", phrase, prompt) }
    }
}
'''
    p.write_text(s[:start]+test_func+s[end:])
    (ROOT/"scripts"/"generate_ocr_prompt_array.py").write_text(GENERATOR)
    patch_text(APISERVER/"openai_prompt_image.go",'//go:embed ocr_prompt.webp\nvar ocrSystemPromptWebP []byte','//go:embed ocr_prompt_array.webp\nvar ocrSystemPromptWebP []byte','embed')
    patch_text(ROOT/"Dockerfile",'COPY cmd/apiserver/ocr_prompt.webp ./ocr_prompt.webp\n','COPY cmd/apiserver/ocr_prompt.webp ./ocr_prompt.webp\nCOPY cmd/apiserver/ocr_prompt_array.webp ./ocr_prompt_array.webp\n','Dockerfile')
    render_assets(font)

if __name__=="__main__": main()
