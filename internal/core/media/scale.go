package media

import (
	"image"

	"golang.org/x/image/draw"
	"golang.org/x/image/math/f64"
)

// kernelTemporaryBytesPerPixel is what a draw.Kernel scale (CatmullRom,
// BiLinear) allocates per cell of its intermediate buffer: x/image v0.45.0
// draw/scale.go makeTmpBuf sizes it as destination width × source height
// [4]float64 values, 32 B each, whatever the destination height.
const kernelTemporaryBytesPerPixel = 32

// maxKernelTemporaryBytes caps that buffer for the final CatmullRom step. After
// pre-reduction the source is under 2× the destination on each axis, so the
// buffer is under 64 B per output pixel. For the image proxy, 256 MiB keeps
// CatmullRom for every ordinary content_full photo: a 6000×4000 landscape
// needs 1600×2000×32 B = 102 MB, and a 3024×4032 portrait phone photo, which
// pre-reduction does not shrink, needs 1600×4032×32 B = 206 MB (over a 128 MiB
// cap). Cover presets stay under 64 MB. Only a tall Height:0 contain with more
// than 5,242 source rows left after pre-reduction exceeds the cap. ScaleImage
// scales it with ApproxBiLinear, which at a ratio under 2× blends the nearest
// 2×2 source pixels: some aliasing on fine detail, accepted for these rare
// proxy shapes. Near 1:1, the shape of an image just wider than the preset,
// that aliasing is negligible. Uploads exceed the cap far more often (see
// scaleUploadImage) and do not fall back to ApproxBiLinear.
const maxKernelTemporaryBytes = 256 << 20

// boxReduce halves each axis of sourceRect of source with ApproxBiLinear while
// it is at least 2× its target, which at an exact 2:1 ratio samples at 2x+0.5
// and so averages each 2×2 (or 2×1) block: a box mipmap with no float buffer.
// Each halving keeps the axis at or above its target, so the returned source
// is under 2× width × height on each axis.
func boxReduce(source image.Image, sourceRect image.Rectangle, width, height int) (image.Image, image.Rectangle) {
	for {
		reducedWidth, reducedHeight := sourceRect.Dx(), sourceRect.Dy()
		if reducedWidth >= 2*width {
			reducedWidth /= 2
		}
		if reducedHeight >= 2*height {
			reducedHeight /= 2
		}
		if reducedWidth == sourceRect.Dx() && reducedHeight == sourceRect.Dy() {
			return source, sourceRect
		}
		reduced := image.NewRGBA(image.Rect(0, 0, reducedWidth, reducedHeight))
		draw.ApproxBiLinear.Scale(reduced, reduced.Bounds(), source, sourceRect, draw.Src, nil)
		source, sourceRect = reduced, reduced.Bounds()
	}
}

// kernelBufferFits reports whether a draw.Kernel scale from sourceRect to a
// width-pixel-wide destination fits maxKernelTemporaryBytes.
func kernelBufferFits(sourceRect image.Rectangle, width int) bool {
	return int64(width)*int64(sourceRect.Dy())*kernelTemporaryBytesPerPixel <= maxKernelTemporaryBytes
}

// ScaleImage scales sourceRect of source to a width × height RGBA image for
// the image proxy. After box pre-reduction (boxReduce) the last step is a
// reduction of under 2×, which uses CatmullRom when its temporary buffer fits
// maxKernelTemporaryBytes and ApproxBiLinear otherwise.
func ScaleImage(source image.Image, sourceRect image.Rectangle, width, height int) *image.RGBA {
	source, sourceRect = boxReduce(source, sourceRect, width, height)
	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	var interpolator draw.Interpolator = draw.CatmullRom
	if !kernelBufferFits(sourceRect, width) {
		interpolator = draw.ApproxBiLinear
	}
	interpolator.Scale(dst, dst.Bounds(), source, sourceRect, draw.Src, nil)
	return dst
}

// scaleUploadImage scales source to a width × height RGBA image for storage.
// An upload is kept permanently and most downscales to a 4096-pixel long edge
// leave more than 2,048 source rows after pre-reduction, over the kernel
// buffer cap, so its last step is always CatmullRom: Scale when the buffer
// fits, otherwise Transform with the same source-to-destination mapping.
// Transform widens the kernel when shrinking, as Scale does, but computes each
// destination pixel straight from the source, so it needs no buffer and is
// slower.
func scaleUploadImage(source image.Image, width, height int) *image.RGBA {
	source, sourceRect := boxReduce(source, source.Bounds(), width, height)
	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	if kernelBufferFits(sourceRect, width) {
		draw.CatmullRom.Scale(dst, dst.Bounds(), source, sourceRect, draw.Src, nil)
		return dst
	}
	scaleX := float64(width) / float64(sourceRect.Dx())
	scaleY := float64(height) / float64(sourceRect.Dy())
	sourceToDestination := f64.Aff3{
		scaleX, 0, -float64(sourceRect.Min.X) * scaleX,
		0, scaleY, -float64(sourceRect.Min.Y) * scaleY,
	}
	draw.CatmullRom.Transform(dst, sourceToDestination, source, sourceRect, draw.Src, nil)
	return dst
}
