# Generates assets/ddmail.ico — emerald rounded square with a white speech
# bubble (a short "written message": two emerald text lines inside), tail bottom-left.
# Vector source of truth is assets/ddmail.svg; this reproduces it in GDI+ so the
# multi-size ICO has crisp per-size rasters (PNG-compressed entries, valid since Vista).
# Rerun after changing the design: powershell -File installer/make_icon.ps1
Add-Type -AssemblyName System.Drawing

$EMERALD = [System.Drawing.Color]::FromArgb(255, 16, 185, 129)   # #10b981

function New-RoundedPath([float]$x, [float]$y, [float]$w, [float]$h, [float]$r) {
    $p = New-Object System.Drawing.Drawing2D.GraphicsPath
    $d = $r * 2
    $p.AddArc($x, $y, $d, $d, 180, 90)
    $p.AddArc($x + $w - $d, $y, $d, $d, 270, 90)
    $p.AddArc($x + $w - $d, $y + $h - $d, $d, $d, 0, 90)
    $p.AddArc($x, $y + $h - $d, $d, $d, 90, 90)
    $p.CloseFigure()
    return $p
}

# Draws the icon of edge $s into a fresh bitmap.
function New-Icon([int]$s) {
    $bmp = New-Object System.Drawing.Bitmap($s, $s)
    $g = [System.Drawing.Graphics]::FromImage($bmp)
    $g.SmoothingMode = [System.Drawing.Drawing2D.SmoothingMode]::AntiAlias
    $g.Clear([System.Drawing.Color]::Transparent)

    # Emerald rounded tile.
    $tile = New-RoundedPath 0 0 ($s - 1) ($s - 1) ([Math]::Max(2, $s * 0.22))
    $g.FillPath((New-Object System.Drawing.SolidBrush($EMERALD)), $tile)

    # White speech-bubble body + tail.
    $bx = $s * 0.20; $by = $s * 0.24; $bw = $s * 0.60; $bh = $s * 0.40
    $body = New-RoundedPath $bx $by $bw $bh ($s * 0.13)
    $white = New-Object System.Drawing.SolidBrush([System.Drawing.Color]::White)
    $bottom = $by + $bh
    $tail = New-Object System.Drawing.Drawing2D.GraphicsPath
    $tail.AddPolygon(@(
        (New-Object System.Drawing.PointF(($bx + $bw * 0.22), ($bottom - 1))),
        (New-Object System.Drawing.PointF(($bx + $bw * 0.18), ($bottom + $s * 0.12))),
        (New-Object System.Drawing.PointF(($bx + $bw * 0.48), ($bottom - 1)))
    ))
    $g.FillPath($white, $body)
    $g.FillPath($white, $tail)

    # Two emerald "text" lines — a written message inside the bubble.
    $lb = New-Object System.Drawing.SolidBrush($EMERALD)
    $lh = [Math]::Max(1.0, $s * 0.055)
    $lx = $bx + $bw * 0.18
    $g.FillPath($lb, (New-RoundedPath $lx ($by + $bh * 0.30) ($bw * 0.64) $lh ($lh / 2)))
    $g.FillPath($lb, (New-RoundedPath $lx ($by + $bh * 0.56) ($bw * 0.44) $lh ($lh / 2)))

    $g.Dispose()
    return $bmp
}

$sizes = 16, 24, 32, 48, 64, 128, 256
$pngs = @()
foreach ($s in $sizes) {
    $bmp = New-Icon $s
    $ms = New-Object System.IO.MemoryStream
    $bmp.Save($ms, [System.Drawing.Imaging.ImageFormat]::Png)
    $pngs += , @($s, $ms.ToArray())
    $bmp.Dispose()
}

# Assemble the ICO: ICONDIR + ICONDIRENTRY[] + PNG blobs.
$out = New-Object System.IO.MemoryStream
$w = New-Object System.IO.BinaryWriter($out)
$w.Write([uint16]0); $w.Write([uint16]1); $w.Write([uint16]$pngs.Count)
$offset = 6 + 16 * $pngs.Count
foreach ($p in $pngs) {
    $s = $p[0]; $bytes = $p[1]
    $dim = if ($s -ge 256) { 0 } else { $s }
    $w.Write([byte]$dim); $w.Write([byte]$dim)   # width, height (0 = 256)
    $w.Write([byte]0); $w.Write([byte]0)          # palette, reserved
    $w.Write([uint16]1); $w.Write([uint16]32)     # planes, bpp
    $w.Write([uint32]$bytes.Length); $w.Write([uint32]$offset)
    $offset += $bytes.Length
}
foreach ($p in $pngs) { $w.Write($p[1]) }
$w.Flush()

$dest = Join-Path $PSScriptRoot "..\assets\ddmail.ico"
[System.IO.File]::WriteAllBytes($dest, $out.ToArray())
Write-Host "Wrote $dest ($($out.Length) bytes, $($pngs.Count) sizes)"

# Also emit a 256x256 PNG. Bundled into the exe (include_bytes!) and decoded at
# runtime for the Slint window icon (WM_SETICON) and the system-tray glyph —
# neither of which reads the embedded .ico.
$png = New-Icon 256
$pngDest = Join-Path $PSScriptRoot "..\assets\ddmail_icon.png"
$png.Save($pngDest, [System.Drawing.Imaging.ImageFormat]::Png)
$png.Dispose()
Write-Host "Wrote $pngDest"
