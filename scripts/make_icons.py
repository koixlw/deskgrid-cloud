"""Generate the deskgrid-cloud icons (.ico) used by both the worker .exe and
the Go gateway .exe.

Design language (shared):
- Dark canvas #0e1117 (matches web/style.css)
- 4x4 grid of rounded squares
- Accent color #58a6ff for "active"
- Dim color #8b949e (80% alpha) for "idle"

Differentiation:
- Worker: a few cells lit at scattered positions => "a fleet, some busy,
  some idle". Outer border thin.
- Gateway: ALL 16 cells lit + slightly thicker outer ring => "central
  orchestrator that sees all nodes". The thicker rim reads as "this is
  the control plane" vs the worker's "data plane".
- Client: 4 corner cells lit (the four remote dispatch targets) with the
  top-right corner brightest (the "primary offload target"), plus a
  small outward arrow at the top-right edge suggesting "dispatch
  elsewhere". Outer border thin (matches worker — both are "data
  plane" from the gateway's perspective).

Outputs:
- assets/deskgrid-worker.ico
- assets/deskgrid-gateway.ico
- assets/deskgrid-client.ico
All three contain 16/24/32/48/64/128/256.
"""
import os
from PIL import Image, ImageDraw

# ---------- Color palette (matches web/style.css) ----------
BG          = (14, 17, 23, 255)        # #0e1117
SQUARE_DIM  = (139, 148, 158, 200)     # #8b949e at 80% alpha
SQUARE_LIT  = (88, 166, 255, 255)      # #58a6ff
SQUARE_BRIGHT = (200, 220, 255, 255)   # brighter blue (gateway center emphasis)
GLOW        = (88, 166, 255, 80)       # halo around lit squares
BORDER      = (33, 38, 45, 255)        # #21262d
ARROW       = (200, 220, 255, 255)     # match the "bright" color so it pops

# ---------- Layout constants (canvas-relative) ----------
SIZE = 256
GRID_N = 4
PAD = 28
CELL_GAP = 8
CELL_SIZE = (SIZE - 2 * PAD - (GRID_N - 1) * CELL_GAP) // GRID_N  # ~46
CORNER_R = 6

# Worker: scattered "busy" cells
WORKER_LIT = {(0, 1), (1, 0), (1, 1), (2, 2), (3, 0)}
WORKER_BORDER_PX = 1

# Gateway: everything lit, center 2x2 brighter, thicker rim
GATEWAY_LIT = {(r, c) for r in range(GRID_N) for c in range(GRID_N)}
GATEWAY_BRIGHT = {(1, 1), (1, 2), (2, 1), (2, 2)}  # 2x2 center
GATEWAY_BORDER_PX = 2

# Client: 4 corner cells lit (4 dispatch targets). Top-right brightest
# because that is the visual direction the offload arrow points to.
CLIENT_LIT = {(0, 0), (0, 3), (3, 0), (3, 3)}
CLIENT_BRIGHT = {(0, 3)}
CLIENT_BORDER_PX = 1


def render(canvas_size: int, lit: set, bright: set | None = None,
           border_px: int = 1, overlay_arrow: bool = False) -> Image.Image:
    """Draw one icon at the given canvas size (used for each ICO sub-image).

    When overlay_arrow is True, paint a small outward arrow at the
    top-right edge — the "dispatch elsewhere" mark, used for client.
    """
    img = Image.new("RGBA", (canvas_size, canvas_size), BG)
    draw = ImageDraw.Draw(img)

    scale = canvas_size / SIZE
    pad = int(PAD * scale)
    cell_gap = int(CELL_GAP * scale)
    cell_size = int(CELL_SIZE * scale)
    corner = max(2, int(CORNER_R * scale))
    halo = max(2, int(4 * scale))
    border = max(1, int(border_px * scale))

    # Outer border
    draw.rectangle((0, 0, canvas_size - 1, canvas_size - 1), outline=BORDER, width=border)

    for row in range(GRID_N):
        for col in range(GRID_N):
            x0 = pad + col * (cell_size + cell_gap)
            y0 = pad + row * (cell_size + cell_gap)
            x1 = x0 + cell_size
            y1 = y0 + cell_size

            if (row, col) in lit:
                # Halo behind lit squares
                draw.rounded_rectangle(
                    (x0 - halo, y0 - halo, x1 + halo, y1 + halo),
                    radius=corner + halo, fill=GLOW,
                )
                color = SQUARE_BRIGHT if (bright and (row, col) in bright) else SQUARE_LIT
                draw.rounded_rectangle(
                    (x0, y0, x1, y1), radius=corner, fill=color,
                )
            else:
                draw.rounded_rectangle(
                    (x0, y0, x1, y1), radius=corner, fill=SQUARE_DIM,
                )

    if overlay_arrow:
        # Outward arrow at top-right edge — "dispatch elsewhere"
        # shaft: small bar from the (0,3) lit square toward the upper-right corner
        ax0 = int(SIZE * 0.58 * scale)
        ay0 = int(SIZE * 0.42 * scale)
        ax1 = int(SIZE * 0.92 * scale)
        ay1 = int(SIZE * 0.08 * scale)
        line_w = max(2, int(6 * scale))
        # Shaft
        draw.line((ax0, ay0, ax1, ay1), fill=ARROW, width=line_w)
        # Arrow head (triangle) at the (ax1, ay1) tip
        head = max(4, int(14 * scale))
        draw.polygon(
            [
                (ax1, ay1),
                (ax1 - head, ay1 + head // 2),
                (ax1 - head // 2, ay1 + head),
            ],
            fill=ARROW,
        )

    return img


def write_ico(out_path: str, lit: set, bright: set | None = None,
              border_px: int = 1, overlay_arrow: bool = False,
              sizes=(16, 24, 32, 48, 64, 128, 256)) -> None:
    base = render(SIZE, lit, bright, border_px, overlay_arrow=overlay_arrow)
    base.save(out_path, format="ICO", sizes=[(s, s) for s in sizes])
    print(f"[icon] {out_path}  ({os.path.getsize(out_path):,} bytes, sizes={sizes})")


def main():
    out_dir = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "assets"))
    os.makedirs(out_dir, exist_ok=True)

    write_ico(os.path.join(out_dir, "deskgrid-worker.ico"),
              lit=WORKER_LIT, border_px=WORKER_BORDER_PX)
    write_ico(os.path.join(out_dir, "deskgrid-gateway.ico"),
              lit=GATEWAY_LIT, bright=GATEWAY_BRIGHT,
              border_px=GATEWAY_BORDER_PX)
    write_ico(os.path.join(out_dir, "deskgrid-client.ico"),
              lit=CLIENT_LIT, bright=CLIENT_BRIGHT,
              border_px=CLIENT_BORDER_PX, overlay_arrow=True)


if __name__ == "__main__":
    main()