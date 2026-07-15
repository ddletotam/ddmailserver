# Generates assets/ddmail.ico — brand-blue rounded square with white "dd".
# Multi-size ICO with PNG-compressed entries (valid since Vista).
# Rerun after changing the design: powershell -File installer/make_icon.ps1
Add-Type -AssemblyName System.Drawing

$sizes = 16, 24, 32, 48, 64, 128, 256
$pngs = @()

foreach ($s in $sizes) {
    $bmp = New-Object System.Drawing.Bitmap($s, $s)
    $g = [System.Drawing.Graphics]::FromImage($bmp)
    $g.SmoothingMode = [System.Drawing.Drawing2D.SmoothingMode]::AntiAlias
    $g.TextRenderingHint = [System.Drawing.Text.TextRenderingHint]::AntiAlias
    $g.Clear([System.Drawing.Color]::Transparent)

    # Rounded square, brand blue #2f80ed (same as the tray icon).
    $r = [Math]::Max(2, [int]($s * 0.22))
    $path = New-Object System.Drawing.Drawing2D.GraphicsPath
    $rect = New-Object System.Drawing.Rectangle(0, 0, ($s - 1), ($s - 1))
    $d = $r * 2
    $path.AddArc($rect.X, $rect.Y, $d, $d, 180, 90)
    $path.AddArc($rect.Right - $d, $rect.Y, $d, $d, 270, 90)
    $path.AddArc($rect.Right - $d, $rect.Bottom - $d, $d, $d, 0, 90)
    $path.AddArc($rect.X, $rect.Bottom - $d, $d, $d, 90, 90)
    $path.CloseFigure()
    $brush = New-Object System.Drawing.SolidBrush([System.Drawing.Color]::FromArgb(255, 47, 128, 237))
    $g.FillPath($brush, $path)

    # White "dd", bold, optically centered.
    $fontSize = [Math]::Max(6, $s * 0.42)
    $font = New-Object System.Drawing.Font("Segoe UI", $fontSize, [System.Drawing.FontStyle]::Bold, [System.Drawing.GraphicsUnit]::Pixel)
    $fmt = New-Object System.Drawing.StringFormat
    $fmt.Alignment = [System.Drawing.StringAlignment]::Center
    $fmt.LineAlignment = [System.Drawing.StringAlignment]::Center
    $textRect = New-Object System.Drawing.RectangleF(0, ($s * -0.02), $s, $s)
    $g.DrawString("dd", $font, [System.Drawing.Brushes]::White, $textRect, $fmt)

    $ms = New-Object System.IO.MemoryStream
    $bmp.Save($ms, [System.Drawing.Imaging.ImageFormat]::Png)
    $pngs += , @($s, $ms.ToArray())
    $g.Dispose(); $bmp.Dispose()
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
