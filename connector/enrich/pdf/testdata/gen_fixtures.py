#!/usr/bin/env python3
"""Generate the PDF fixtures used by pdf_test.go (enrichruntime build tag).

Why hand-rolled PDFs instead of pulling in reportlab/fpdf as a build dep?
The fixtures need to be tiny, deterministic, and reviewable in a code
review. PDF 1.4 with no compression, no images, plain ASCII is small
enough to commit literally and survives any future poppler-utils upgrade.

Run from this directory:

    python3 gen_fixtures.py

Outputs:
    text.pdf         single-page PDF with extractable "Hello, glovebox..." text
    image-only.pdf   single-page PDF with no text stream
    encrypted.pdf    minimal PDF carrying an Encrypt dict (poppler rejects)
    corrupt.pdf      truncated bytes (no %%EOF, broken xref)

The script is deliberately ASCII-only - see project conventions.
"""

import os


def build_pdf(objects, root_obj):
    """Assemble a minimal PDF from a list of (object_number, body) tuples.

    Returns the PDF bytes. Builds the xref table and trailer from scratch.
    """
    header = b"%PDF-1.4\n%\xe2\xe3\xcf\xd3\n"
    body = bytearray(header)
    offsets = {0: 0}
    for num, content in objects:
        offsets[num] = len(body)
        body += f"{num} 0 obj\n".encode("ascii")
        body += content
        if not content.endswith(b"\n"):
            body += b"\n"
        body += b"endobj\n"

    xref_offset = len(body)
    num_objects = max(offsets) + 1
    body += f"xref\n0 {num_objects}\n".encode("ascii")
    body += b"0000000000 65535 f \n"
    for n in range(1, num_objects):
        body += f"{offsets[n]:010d} 00000 n \n".encode("ascii")

    body += b"trailer\n"
    body += f"<< /Size {num_objects} /Root {root_obj} 0 R >>\n".encode("ascii")
    body += f"startxref\n{xref_offset}\n%%EOF\n".encode("ascii")
    return bytes(body)


def build_text_pdf():
    """A single page with the text 'Hello, glovebox PDF enricher test.'."""
    content_stream = (
        b"BT\n"
        b"/F1 12 Tf\n"
        b"72 720 Td\n"
        b"(Hello, glovebox PDF enricher test.) Tj\n"
        b"ET\n"
    )

    objects = [
        (1, b"<< /Type /Catalog /Pages 2 0 R >>"),
        (2, b"<< /Type /Pages /Kids [3 0 R] /Count 1 >>"),
        (
            3,
            b"<< /Type /Page /Parent 2 0 R "
            b"/MediaBox [0 0 612 792] "
            b"/Resources << /Font << /F1 5 0 R >> >> "
            b"/Contents 4 0 R >>",
        ),
        (
            4,
            f"<< /Length {len(content_stream)} >>\nstream\n".encode("ascii")
            + content_stream
            + b"endstream",
        ),
        (5, b"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"),
    ]
    return build_pdf(objects, root_obj=1)


def build_image_only_pdf():
    """A page with no text - empty content stream. pdftotext will emit
    nothing for the page (modulo a trailing form feed, which the test
    strips)."""
    content_stream = b""
    objects = [
        (1, b"<< /Type /Catalog /Pages 2 0 R >>"),
        (2, b"<< /Type /Pages /Kids [3 0 R] /Count 1 >>"),
        (
            3,
            b"<< /Type /Page /Parent 2 0 R "
            b"/MediaBox [0 0 612 792] "
            b"/Resources << >> "
            b"/Contents 4 0 R >>",
        ),
        (
            4,
            f"<< /Length {len(content_stream)} >>\nstream\n".encode("ascii")
            + content_stream
            + b"endstream",
        ),
    ]
    return build_pdf(objects, root_obj=1)


def build_encrypted_pdf():
    """A minimal PDF with an /Encrypt dictionary. We do NOT actually
    encrypt the content streams - we just expose the Encrypt entry so
    poppler treats this as an encrypted document. With no /U or valid
    /O/P entries, pdftotext refuses to extract without -upw / -opw.
    """
    content_stream = b"BT /F1 12 Tf 72 720 Td (secret) Tj ET\n"
    objects = [
        (1, b"<< /Type /Catalog /Pages 2 0 R >>"),
        (2, b"<< /Type /Pages /Kids [3 0 R] /Count 1 >>"),
        (
            3,
            b"<< /Type /Page /Parent 2 0 R "
            b"/MediaBox [0 0 612 792] "
            b"/Resources << /Font << /F1 5 0 R >> >> "
            b"/Contents 4 0 R >>",
        ),
        (
            4,
            f"<< /Length {len(content_stream)} >>\nstream\n".encode("ascii")
            + content_stream
            + b"endstream",
        ),
        (5, b"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"),
        # Object 6 is the Encrypt dict. Standard security handler v1,
        # 40-bit RC4 placeholder. The U/O strings are 32 zero bytes,
        # which will not validate against an empty password - poppler
        # will emit "Incorrect password" / "Encrypted" diagnostics.
        (
            6,
            b"<< /Filter /Standard /V 1 /R 2 /Length 40 "
            b"/P -64 "
            b"/O <" + b"00" * 32 + b"> "
            b"/U <" + b"00" * 32 + b"> "
            b">>",
        ),
    ]
    # We need to include /Encrypt and /ID in the trailer. The base
    # build_pdf builder does not, so we re-do the trailer here.
    header = b"%PDF-1.4\n%\xe2\xe3\xcf\xd3\n"
    body = bytearray(header)
    offsets = {0: 0}
    for num, content in objects:
        offsets[num] = len(body)
        body += f"{num} 0 obj\n".encode("ascii")
        body += content
        if not content.endswith(b"\n"):
            body += b"\n"
        body += b"endobj\n"

    xref_offset = len(body)
    num_objects = max(offsets) + 1
    body += f"xref\n0 {num_objects}\n".encode("ascii")
    body += b"0000000000 65535 f \n"
    for n in range(1, num_objects):
        body += f"{offsets[n]:010d} 00000 n \n".encode("ascii")

    body += b"trailer\n"
    body += (
        b"<< /Size "
        + str(num_objects).encode("ascii")
        + b" /Root 1 0 R /Encrypt 6 0 R "
        b"/ID [<00112233445566778899AABBCCDDEEFF> "
        b"<00112233445566778899AABBCCDDEEFF>] >>\n"
    )
    body += f"startxref\n{xref_offset}\n%%EOF\n".encode("ascii")
    return bytes(body)


def build_corrupt_pdf():
    """Truncated bytes - looks like a PDF header but the xref/trailer
    are missing. pdftotext returns a non-zero exit code."""
    return b"%PDF-1.4\n%\xe2\xe3\xcf\xd3\n1 0 obj\n<< /Type /Catalog "


def main():
    here = os.path.dirname(os.path.abspath(__file__))
    files = {
        "text.pdf": build_text_pdf(),
        "image-only.pdf": build_image_only_pdf(),
        "encrypted.pdf": build_encrypted_pdf(),
        "corrupt.pdf": build_corrupt_pdf(),
    }
    for name, data in files.items():
        path = os.path.join(here, name)
        with open(path, "wb") as fh:
            fh.write(data)
        print(f"wrote {name} ({len(data)} bytes)")


if __name__ == "__main__":
    main()
