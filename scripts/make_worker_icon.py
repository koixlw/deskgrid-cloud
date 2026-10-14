"""Generate the deskgrid-cloud worker .ico icon.

Design: 4x4 grid of squares (representing a fleet of PCs), a few lit up
in accent blue, others dim — evokes "compute nodes, some busy, some idle".
Matches the dashboard palette (bg #0e1117, accent #58a6ff).

Output: assets/deskgrid-worker.ico (multi-resolution: 16/24/32/48/64/128/256)
"""
import os
import sys
from PIL import Image, ImageDraw

# ---------- Color palette (matches web/style.css) ----------
BG          = (14, 17, 23, 255)        # #0e1117
SQUARE_DIM  = (139, 148, 158, 200)     # #8b949e at 80% alpha
SQUARE_LIT  = (88, 166, 255, 255)      # #58a6ff
GLOW        = (88, 166, 255, 80)       # glow halo around lit squares
BORDER      = (33, 38, 45, 255)        # #21262d

# ---------- Layout (drawn on a 256x256 canvas) ----------
SIZE = 256
GRID_N = 4  # 4x4 grid
PAD = 28
CELL_GAP = 8
CELL_SIZE = (SIZE - 2 * PAD - (GRID_N - 1) * CELL_GAP) // GRID_N  # ~46
# Round cells + halo
CORNER_R = 6

# Pattern: which cells are bright. Diagonal accent + a couple off-diagonal
# so it looks "alive" but not symmetric.
LIT = {
    (0, 1), (1, 0), (1, 1),
    (2, 2), (3, 0),
}


def render(canvas_size: int) -> Image.Image:
    img = Image.new("RGBA", (canvas_size, canvas_size), BG)
    draw = ImageDraw.Draw(img)

    # Subtle border (1px ring)
    draw.rectangle((0, 0, canvas_size - 1, canvas_size - 1), outline=BORDER)

    scale = canvas_size / SIZE
    pad = int(PAD * scale)
    cell_gap = int(CELL_GAP * scale)
    cell_size = int(CELL_SIZE * scale)
    corner = max(2, int(CORNER_R * scale))

    for row in range(GRID_N):
        for col in range(GRID_N):
            x0 = pad + col * (cell_size + cell_gap)
            y0 = pad + row * (cell_size + cell_gap)
            x1 = x0 + cell_size
            y1 = y0 + cell_size
            if (row, col) in LIT:
                # Halo behind the lit square
                halo = max(2, int(4 * scale))
                draw.rounded_rectangle(
                    (x0 - halo, y0 - halo, x1 + halo, y1 + halo),
                    radius=corner + halo, fill=GLOW,
                )
                draw.rounded_rectangle(
                    (x0, y0, x1, y1), radius=corner, fill=SQUARE_LIT,
                )
            else:
                draw.rounded_rectangle(
                    (x0, y0, x1, y1), radius=corner, fill=SQUARE_DIM,
                )
    return img


def main():
    out_dir = os.path.join(os.path.dirname(__file__), "..", "assets")
    out_path = os.path.abspath(os.path.join(out_dir, "deskgrid-worker.ico"))
    os.makedirs(out_dir, exist_ok=True)

    base = render(SIZE)
    sizes = [16, 24, 32, 48, 64, 128, 256]
    base.save(out_path, format="ICO", sizes=[(s, s) for s in sizes])
    print(f"[icon] wrote {out_path} ({os.path.getsize(out_path):,} bytes)")
    print(f"[icon] sizes: {sizes}")


if __name__ == "__main__":
    main()