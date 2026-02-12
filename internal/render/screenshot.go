package render

import (
	"bytes"
	_ "embed"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fogleman/gg"
	"github.com/golang/freetype/truetype"
	"github.com/guohuiyuan/qzonewall-go/internal/model"
	"golang.org/x/image/draw"
	"golang.org/x/image/font"
)

//go:embed font.ttf
var fontData []byte

type Renderer struct {
	font *truetype.Font
}

func NewRenderer() *Renderer {
	f, err := truetype.Parse(fontData)
	if err != nil {
		log.Printf("[Renderer] ❌ 严重错误: 内置字体解析失败: %v", err)
		return &Renderer{font: nil}
	}
	return &Renderer{font: f}
}

func (r *Renderer) Available() bool {
	return r.font != nil
}

func (r *Renderer) getFace(size float64) font.Face {
	if r.font == nil {
		return nil
	}
	return truetype.NewFace(r.font, &truetype.Options{
		Size:    size,
		DPI:     72,
		Hinting: font.HintingFull,
	})
}

// RenderPost 渲染图文合一
func (r *Renderer) RenderPost(post *model.Post) ([]byte, error) {
	if !r.Available() {
		return nil, fmt.Errorf("渲染器未初始化(字体缺失)")
	}

	// ── 1. 样式配置 ──
	const (
		CanvasWidth = 800.0
		Padding     = 40.0
		SizeText    = 32.0
		SizeName    = 28.0
		SizeMeta    = 22.0
		AvatarSize  = 90.0
		AvatarRight = 20.0
		BubblePadH  = 30.0
		BubblePadV  = 25.0
		LineHeight  = 1.4
		ImgGap      = 10.0
		ImgSize     = 220.0
	)

	// ── 2. 计算布局 ──
	hasAvatar := !post.Anon
	contentMaxW := CanvasWidth - (Padding * 2)
	if hasAvatar {
		contentMaxW -= AvatarSize + AvatarRight
	}
	
	measureDc := gg.NewContext(1, 1)
	textFace := r.getFace(SizeText)
	measureDc.SetFontFace(textFace)

	var lines []string
	if post.Text != "" {
		lines = measureDc.WordWrap(post.Text, contentMaxW-(BubblePadH*2))
	}

	fontH := measureDc.FontHeight()
	bubbleH := 0.0
	if len(lines) > 0 {
		textBlockH := float64(len(lines)) * fontH * LineHeight
		bubbleH = textBlockH + (BubblePadV * 2)
	}

	imgAreaH := 0.0
	imgCount := len(post.Images)
	var imgCols, imgRows int

	if imgCount > 0 {
		if imgCount == 1 {
			imgCols = 1
			// imgRows = 1 (修复: 单图模式不需要计算行数，避免 lint 报错)
			imgAreaH = 400.0 // 单张限高
		} else {
			imgCols = 3
			if imgCount == 2 || imgCount == 4 {
				imgCols = 2
			}
			imgRows = int(math.Ceil(float64(imgCount) / float64(imgCols)))
			imgAreaH = float64(imgRows)*ImgSize + float64(imgRows-1)*ImgGap
		}
	}

	currentY := Padding
	currentY += SizeName + 15
	contentStartY := currentY

	if bubbleH > 0 {
		currentY += bubbleH
	}
	if imgAreaH > 0 {
		if bubbleH > 0 {
			currentY += 20.0
		}
		currentY += imgAreaH
	}
	currentY += 50.0

	totalH := int(currentY)
	minH := Padding + Padding
	if hasAvatar {
		minH = Padding + AvatarSize + Padding
	}
	if totalH < int(minH) {
		totalH = int(minH)
	}

	// ── 3. 开始绘制 ──
	dc := gg.NewContext(int(CanvasWidth), totalH)
	dc.SetHexColor("#F5F5F5") 
	dc.Clear()

	startX := Padding
	startY := Padding

	// 3.1 绘制头像（匿名投稿不渲染）
	contentX := startX
	if hasAvatar {
		avatarImg := downloadAndResize(post.QQAvatarURL(), int(AvatarSize), int(AvatarSize))
		dc.Push()
		dc.DrawCircle(startX+AvatarSize/2, startY+AvatarSize/2, AvatarSize/2)
		dc.Clip()
		if avatarImg != nil {
			dc.DrawImageAnchored(avatarImg, int(startX+AvatarSize/2), int(startY+AvatarSize/2), 0.5, 0.5)
		} else {
			dc.SetHexColor("#DCDCDC")
			dc.DrawRectangle(startX, startY, AvatarSize, AvatarSize)
			dc.Fill()
		}
		dc.Pop()
		// ★★★ 核心修复：强制重置裁剪区域，否则后面画的内容都会不可见 ★★★
		dc.ResetClip()
		contentX = startX + AvatarSize + AvatarRight
	}

	// 3.2 绘制昵称
	dc.SetFontFace(r.getFace(SizeName))
	dc.SetHexColor("#555555")
	dc.DrawString(post.ShowName(), contentX, startY+SizeName-5)

	currContentY := contentStartY

	// 3.3 绘制文字气泡
	if bubbleH > 0 {
		dc.SetColor(color.White)
		dc.DrawRoundedRectangle(contentX, currContentY, contentMaxW, bubbleH, 16)
		dc.Fill()

		// 小三角
		dc.MoveTo(contentX, currContentY+25)
		dc.LineTo(contentX-10, currContentY+35)
		dc.LineTo(contentX, currContentY+45)
		dc.ClosePath()
		dc.Fill()

		// 文字
		dc.SetFontFace(textFace)
		dc.SetHexColor("#000000")
		
		metrics := textFace.Metrics()
		ascent := float64(metrics.Ascent.Ceil())
		
		textY := currContentY + BubblePadV + ascent
		for i, line := range lines {
			dc.DrawString(line, contentX+BubblePadH, textY+float64(i)*fontH*LineHeight)
		}
		currContentY += bubbleH + 20.0
	}

	// 3.4 绘制图片
	if imgCount > 0 {
		if imgCount == 1 {
			// 单图
			rawImg := downloadImage(post.Images[0])
			if rawImg != nil {
				b := rawImg.Bounds()
				origW, origH := float64(b.Dx()), float64(b.Dy())
				maxW, maxH := 400.0, 500.0
				scale := math.Min(maxW/origW, maxH/origH)
				if scale > 1.0 { scale = 1.0 }
				tw, th := int(origW*scale), int(origH*scale)
				
				finalImg := resizeImage(rawImg, tw, th)

				dc.Push()
				dc.DrawRoundedRectangle(contentX, currContentY, float64(tw), float64(th), 12)
				dc.Clip()
				dc.DrawImage(finalImg, int(contentX), int(currContentY))
				dc.Pop()
				// 这里也要重置，防止影响后续水印
				dc.ResetClip()
			} else {
				// 下载失败显示占位
				drawErrorPlaceholder(dc, contentX, currContentY, 200, 200)
			}
		} else {
			// 九宫格
			for i, imgUrl := range post.Images {
				if i >= 9 { break }
				col := i % imgCols
				row := i / imgCols
				
				ix := contentX + float64(col)*(ImgSize+ImgGap)
				iy := currContentY + float64(row)*(ImgSize+ImgGap)
				
				img := downloadAndResize(imgUrl, int(ImgSize), int(ImgSize))
				if img != nil {
					dc.Push()
					dc.DrawRoundedRectangle(ix, iy, ImgSize, ImgSize, 8)
					dc.Clip()
					dc.DrawImage(img, int(ix), int(iy))
					dc.Pop()
					dc.ResetClip()
				} else {
					drawErrorPlaceholder(dc, ix, iy, ImgSize, ImgSize)
				}
			}
		}
	}

	// 3.5 水印
	dc.SetFontFace(r.getFace(SizeMeta))
	dc.SetHexColor("#AAAAAA")
	dc.DrawStringAnchored(fmt.Sprintf("#%d  %s", post.ID, time.Now().Format("15:04")), CanvasWidth-Padding, float64(totalH)-20, 1.0, 1.0)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dc.Image(), &jpeg.Options{Quality: 90}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// 占位符绘制
func drawErrorPlaceholder(dc *gg.Context, x, y, w, h float64) {
	dc.Push()
	dc.SetHexColor("#E0E0E0")
	dc.DrawRectangle(x, y, w, h)
	dc.Fill()
	dc.SetHexColor("#999999")
	dc.DrawStringAnchored("加载失败", x+w/2, y+h/2, 0.5, 0.5)
	dc.Pop()
}

// 下载工具（带 Header）
func downloadImage(url string) image.Image {
	if url == "" { return nil }
	if local := resolveLocalUploadPath(url); local != "" {
		f, err := os.Open(local)
		if err != nil {
			log.Printf("加载本地图片失败: %v | path: %s", err, local)
			return nil
		}
		defer f.Close()
		img, _, err := image.Decode(f)
		if err != nil {
			log.Printf("解析本地图片失败: %v | path: %s", err, local)
			return nil
		}
		return img
	}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil { return nil }
	// 伪装浏览器防止 403
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil { 
		log.Printf("下载出错: %v | URL: %s", err, url)
		return nil 
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 { return nil }
	img, _, err := image.Decode(resp.Body)
	if err != nil { return nil }
	return img
}

func resolveLocalUploadPath(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	local := strings.TrimPrefix(raw, "/")
	local = strings.TrimPrefix(local, "./")
	local = filepath.Clean(local)
	prefix := "uploads" + string(filepath.Separator)
	if local == "uploads" || strings.HasPrefix(local, prefix) {
		return local
	}
	return ""
}

func resizeImage(src image.Image, w, h int) image.Image {
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Over, nil)
	return dst
}

func downloadAndResize(url string, w, h int) image.Image {
	src := downloadImage(url)
	if src == nil { return nil }
	if w == h {
		dst := image.NewRGBA(image.Rect(0, 0, w, h))
		draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Over, nil)
		return dst
	}
	return resizeImage(src, w, h)
}
