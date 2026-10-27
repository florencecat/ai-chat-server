package report

import (
	"bytes"
	"embed"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/phpdave11/gofpdf"
)

// Шрифты и логотип вшиты в бинарник: в образе на alpine нет ни Nunito, ни
// Rubik, а без TTF gofpdf не умеет кириллицу. Иконки — шрифт Phosphor, тот же
// набор, что в макете.
//
//go:embed assets/fonts/*.ttf assets/logo.png
var assets embed.FS

// Вся вёрстка ведётся в CSS-пикселях макета (96 dpi), в пункты PDF переводит
// pt(). Так размеры можно переносить из макета один в один.
const (
	pageW    = 793.7
	pageH    = 1122.5
	margin   = 67.2 // 0.7in
	contentW = pageW - 2*margin
	bottom   = pageH - margin
)

func pt(px float64) float64 { return px * 0.75 }

// ── Палитра: токены дизайн-системы (tokens/colors.css) ───────────────────────

type rgb struct{ r, g, b int }

func hex(s string) rgb {
	v, _ := strconv.ParseUint(strings.TrimPrefix(s, "#"), 16, 32)
	return rgb{int(v >> 16 & 0xff), int(v >> 8 & 0xff), int(v & 0xff)}
}

var (
	white     = hex("#ffffff")
	violet50  = hex("#f3eefc")
	violet100 = hex("#ece2fb")
	violet200 = hex("#d8cbf6")
	violet300 = hex("#c9b6f5")
	violet500 = hex("#8a6fe0")
	violet600 = hex("#7a5fd0")
	violet700 = hex("#6f50c8")
	mint100   = hex("#e0f4ec")
	mint200   = hex("#d8f1e6")
	mint500   = hex("#3fae8f")
	mint600   = hex("#2f8f72")
	coral50   = hex("#fbe9e1")
	coral200  = hex("#f6cdbd")
	coral300  = hex("#f3b39c")
	coral500  = hex("#e8744f")
	coral600  = hex("#c2553a")
	sky50     = hex("#e6eefb")
	sky500    = hex("#5a82d8")
	sky600    = hex("#4a6fc4")
	ink950    = hex("#241a44")
	ink900    = hex("#2b2150")
	ink700    = hex("#3b3168")
	ink600    = hex("#5b527f")
	ink400    = hex("#7a72a0")
	ink300    = hex("#8a82ad")

	textBody  = ink600
	textMuted = ink400
)

// ── Шрифты ────────────────────────────────────────────────────────────────────

const (
	nunito     = "nunito"
	nunitoSemi = "nunito-semi"
	nunitoBold = "nunito-bold"
	rubikBold  = "rubik-bold"
	rubikBlack = "rubik-xbold" // 800 в макете
	icons      = "ph"
	iconsFill  = "ph-fill"
)

var fontFiles = map[string]string{
	nunito:     "Nunito-Regular.ttf",
	nunitoSemi: "Nunito-SemiBold.ttf",
	nunitoBold: "Nunito-Bold.ttf",
	rubikBold:  "Rubik-Bold.ttf",
	rubikBlack: "Rubik-ExtraBold.ttf",
	icons:      "Phosphor-Bold.ttf",
	iconsFill:  "Phosphor-Fill.ttf",
}

// Кодовые точки Phosphor 2.1 (src/bold/style.css).
const (
	icPaw     = ""
	icFlower  = ""
	icPill    = ""
	icSyringe = ""
	icShield  = ""
	icHeart   = ""
	icScales  = ""
	icNote    = ""
	icMale    = ""
	icFemale  = ""
)

// ── Бейджи ────────────────────────────────────────────────────────────────────

type badge struct {
	text   string
	bg, fg rgb
}

var (
	badgeActive  = badge{"активно", mint200, mint600}
	badgePlanned = badge{"запланировано", sky50, sky600}
	badgeValid   = badge{"действует", mint200, mint600}
	badgeExpired = badge{"просрочена", coral50, coral600}
)

func severityBadge(s string) *badge {
	switch s {
	case "mild":
		return &badge{"лёгкий", mint100, mint600}
	case "moderate":
		return &badge{"средний", coral50, coral600}
	case "severe":
		return &badge{"тяжёлый", coral200, coral600}
	}
	return nil
}

// ── Рендер ────────────────────────────────────────────────────────────────────

type doc struct {
	f    *gofpdf.Fpdf
	y    float64
	fam  string
	size float64
	now  time.Time
}

// Render собирает PDF. now задаёт «сегодня» в поясе пользователя: от него
// считаются возраст, статусы прививок и курсов лечения.
func Render(d Data, now time.Time, opt Options) ([]byte, error) {
	f := gofpdf.NewCustom(&gofpdf.InitType{OrientationStr: "P", UnitStr: "pt", SizeStr: "A4"})
	f.SetAutoPageBreak(false, 0)
	f.SetMargins(0, 0, 0)
	f.AliasNbPages("{nb}")
	f.SetTitle("Карточка для ветеринара — "+d.Pet.Name, true)
	f.SetCreator("PetСпутник", true)

	for fam, file := range fontFiles {
		b, err := assets.ReadFile("assets/fonts/" + file)
		if err != nil {
			return nil, fmt.Errorf("font %s: %w", file, err)
		}
		f.AddUTF8FontFromBytes(fam, "", b)
	}
	logo, err := assets.ReadFile("assets/logo.png")
	if err != nil {
		return nil, fmt.Errorf("logo: %w", err)
	}
	f.RegisterImageOptionsReader("logo", gofpdf.ImageOptions{ImageType: "PNG"}, bytes.NewReader(logo))

	p := &doc{f: f, now: now}
	f.SetFooterFunc(p.footer)
	p.addPage()

	p.header()
	p.petCard(d)
	p.alerts(d.Pet)
	p.pills(d.Pills)
	p.vaccines(d.Treatments)
	p.treatments(d.Treatments)
	p.symptoms(d.Symptoms)
	if opt.WeightHistory && len(d.Weights) >= 2 {
		p.weights(d.Weights)
	}
	if opt.VetNotes {
		p.vetNotes()
	}

	if err := f.Error(); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := f.Output(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ── Примитивы ─────────────────────────────────────────────────────────────────

func (p *doc) addPage() {
	p.f.AddPage()
	p.y = margin
}

// ensure переносит блок на новую страницу, если он не помещается целиком —
// аналог break-inside: avoid из макета.
func (p *doc) ensure(h float64) {
	if p.y+h > bottom {
		p.addPage()
	}
}

// gap добавляет отступ сверху, но не в начале страницы.
func (p *doc) gap(h float64) {
	if p.y > margin {
		p.y += h
	}
}

func (p *doc) font(fam string, size float64) {
	p.fam, p.size = fam, size
	p.f.SetFont(fam, "", pt(size))
}

func (p *doc) fill(c rgb)   { p.f.SetFillColor(c.r, c.g, c.b) }
func (p *doc) stroke(c rgb) { p.f.SetDrawColor(c.r, c.g, c.b) }
func (p *doc) color(c rgb)  { p.f.SetTextColor(c.r, c.g, c.b) }

func (p *doc) width(s string) float64 { return p.f.GetStringWidth(s) / 0.75 }

// text пишет строку, вертикально центрированную в строке высотой lh, как это
// делает браузер с line-height.
func (p *doc) text(x, top, lh float64, s string) {
	desc := p.f.GetFontDesc(p.fam, "")
	asc := float64(desc.Ascent) / 1000 * p.size
	des := -float64(desc.Descent) / 1000 * p.size
	p.f.Text(pt(x), pt(top+(lh-asc-des)/2+asc), s)
}

// spaced — текст с letter-spacing (заголовки секций, шапка таблиц). gofpdf не
// умеет межбуквенный интервал, поэтому буквы ставятся по одной.
func (p *doc) spaced(x, top, lh float64, s string, ls float64) float64 {
	start := x
	for _, r := range s {
		ch := string(r)
		p.text(x, top, lh, ch)
		x += p.width(ch) + ls
	}
	return x - start - ls
}

func (p *doc) spacedWidth(s string, ls float64) float64 {
	return p.width(s) + ls*float64(utf8.RuneCountInString(s)-1)
}

// icon рисует глиф Phosphor по центру квадрата size×size.
func (p *doc) icon(glyph string, x, y, box, size float64, c rgb) {
	p.font(icons, size)
	p.color(c)
	p.text(x+(box-p.width(glyph))/2, y, box, glyph)
}

func (p *doc) rrect(x, y, w, h, r float64, style string) {
	r = math.Min(r, math.Min(w, h)/2)
	p.f.RoundedRect(pt(x), pt(y), pt(w), pt(h), pt(r), "1234", style)
}

func (p *doc) hline(x1, x2, y float64, c rgb) {
	p.stroke(c)
	p.f.SetLineWidth(pt(1))
	p.f.Line(pt(x1), pt(y), pt(x2), pt(y))
}

func (p *doc) ellipsize(s string, max float64) string {
	if p.width(s) <= max {
		return s
	}
	r := []rune(s)
	for len(r) > 0 && p.width(string(r)+"…") > max {
		r = r[:len(r)-1]
	}
	return strings.TrimSpace(string(r)) + "…"
}

// wrap разбивает текст по словам текущим шрифтом. firstW — ширина первой
// строки: в ней может стоять жирная подпись вроде «Аллергии:».
func (p *doc) wrap(s string, firstW, restW float64) []string {
	var lines []string
	w := firstW
	push := func(l string) {
		lines = append(lines, l)
		w = restW
	}
	for _, para := range strings.Split(strings.TrimSpace(s), "\n") {
		line := ""
		for _, word := range strings.Fields(para) {
			cand := word
			if line != "" {
				cand = line + " " + word
			}
			if p.width(cand) <= w {
				line = cand
				continue
			}
			if line != "" {
				push(line)
			}
			// Слово длиннее строки режем посимвольно.
			for p.width(word) > w {
				r := []rune(word)
				n := len(r)
				for n > 1 && p.width(string(r[:n])) > w {
					n--
				}
				push(string(r[:n]))
				word = string(r[n:])
			}
			line = word
		}
		push(line)
	}
	return lines
}

func (p *doc) drawBadge(b badge, right, centerY, padX float64) {
	p.font(rubikBold, 11)
	w := p.width(b.text) + 2*padX
	h := 20.0
	x := right - w
	p.fill(b.bg)
	p.rrect(x, centerY-h/2, w, h, h/2, "F")
	p.color(b.fg)
	p.text(x+padX, centerY-h/2, h, b.text)
}

func (p *doc) badgeWidth(b *badge, padX float64) float64 {
	if b == nil {
		return 0
	}
	p.font(rubikBold, 11)
	return p.width(b.text) + 2*padX
}

// ── Шапка и подвал ────────────────────────────────────────────────────────────

func (p *doc) header() {
	const h = 52.0
	y := p.y
	logoY := y + (h-46)/2
	p.f.ClipRoundedRect(pt(margin), pt(logoY), pt(46), pt(46), pt(12), false)
	p.f.ImageOptions("logo", pt(margin), pt(logoY), pt(46), pt(46), false,
		gofpdf.ImageOptions{ImageType: "PNG"}, 0, "")
	p.f.ClipEnd()

	x := margin + 46 + 16
	p.font(rubikBlack, 26)
	p.color(ink950)
	p.text(x, y, 31, "Карточка для ветеринара")
	p.font(nunito, 13)
	p.color(textMuted)
	p.text(x, y+34, 18, "Сформировано в приложении PetСпутник · "+longDate(p.now))
	p.y = y + h
}

func (p *doc) footer() {
	const lh = 14.0
	top := pageH - 14 - lh - 8
	p.f.SetFont(iconsFill, "", pt(12))
	p.fam, p.size = iconsFill, 12
	p.color(violet300)
	p.text(margin, top, lh, icPaw)

	p.font(nunito, 10)
	p.color(ink300)
	p.text(margin+20, top, lh,
		"Документ сформирован автоматически по данным владельца и не заменяет заключение ветеринарного врача.")
	num := fmt.Sprintf("%d / {nb}", p.f.PageNo())
	// Ширина считается по «9 / 9»: алиас {nb} подменяется уже после вёрстки.
	p.text(pageW-margin-p.width("9 / 9"), top, lh, num)
}

// ── Карточка питомца ─────────────────────────────────────────────────────────

func (p *doc) petCard(d Data) {
	const (
		padY, padX = 18.0, 20.0
		avatar     = 56.0
		tileH      = 55.0
		h          = padY + avatar + 14 + tileH + padY
	)
	p.gap(20)
	p.ensure(h)
	x0, y0 := margin, p.y

	p.fill(violet50)
	p.stroke(violet200)
	p.f.SetLineWidth(pt(1))
	p.rrect(x0, y0, contentW, h, 20, "FD")

	// Аватар с лапой.
	ax, ay := x0+padX, y0+padY
	p.fill(white)
	p.stroke(coral300)
	p.f.SetLineWidth(pt(2))
	p.f.Circle(pt(ax+avatar/2), pt(ay+avatar/2), pt(avatar/2-1), "FD")
	p.icon(icPaw, ax, ay, avatar, 28, violet600)

	// Имя, пол, порода.
	tx := ax + avatar + 14
	maxW := x0 + contentW - padX - tx
	ty := ay + (avatar-44)/2
	pet := d.Pet
	p.font(rubikBlack, 20)
	p.color(ink950)
	name := p.ellipsize(pet.Name, maxW-28)
	p.text(tx, ty, 24, name)
	if glyph, bg, fg, ok := genderStyle(pet.Gender); ok {
		gx := tx + p.width(name) + 8
		p.fill(bg)
		p.f.Circle(pt(gx+10), pt(ty+12), pt(10), "F")
		p.icon(glyph, gx, ty+2, 20, 13, fg)
	}

	var sub []string
	if pet.Breed != "" {
		sub = append(sub, pet.Breed)
	}
	if pet.Species != "" {
		sub = append(sub, strings.ToLower(pet.Species))
	}
	if pet.Birthdate != nil {
		sub = append(sub, "дата рождения "+shortDate(*pet.Birthdate))
	}
	p.font(nunito, 13)
	p.color(textBody)
	p.text(tx, ty+26, 18, p.ellipsize(strings.Join(sub, " · "), maxW))

	// Плитки: вес, возраст, кастрация.
	weight, weightCap := "—", "вес не указан"
	if n := len(d.Weights); n > 0 {
		weight = weightText(d.Weights[n-1].Kg)
		weightCap = "вес"
		if t := weightTrend(d.Weights); t != "" {
			weightCap += " · " + t
		}
	}
	ageText := "—"
	if pet.Birthdate != nil {
		ageText = age(*pet.Birthdate, p.now)
	}
	castr, castrCap := castration(pet)

	tiles := [][2]string{{weight, weightCap}, {ageText, "возраст"}, {castr, castrCap}}
	tileW := (contentW - 2*padX - 20) / 3
	tyTile := ay + avatar + 14
	for i, t := range tiles {
		x := x0 + padX + float64(i)*(tileW+10)
		p.fill(white)
		p.rrect(x, tyTile, tileW, tileH, 14, "F")
		p.font(rubikBlack, 16)
		p.color(ink950)
		v := p.ellipsize(t[0], tileW-28)
		p.text(x+(tileW-p.width(v))/2, tyTile+10, 20, v)
		p.font(nunito, 11)
		p.color(textMuted)
		c := p.ellipsize(t[1], tileW-28)
		p.text(x+(tileW-p.width(c))/2, tyTile+30, 15, c)
	}
	p.y = y0 + h
}

func genderStyle(g string) (glyph string, bg, fg rgb, ok bool) {
	switch g {
	case "male":
		return icMale, sky50, sky500, true
	case "female":
		return icFemale, coral50, coral500, true
	}
	return "", rgb{}, rgb{}, false
}

func castration(pet Pet) (string, string) {
	done, word := "Кастрирован", "кастрация"
	if pet.Gender == "female" {
		done, word = "Стерилизована", "стерилизация"
	}
	switch {
	case !pet.Castrated:
		return "Нет", word
	case pet.CastrationDate != nil:
		return done, shortDate(*pet.CastrationDate)
	}
	return done, word
}

// ── Аллергии и хронические заболевания ───────────────────────────────────────

func (p *doc) alerts(pet Pet) {
	const lh = 19.5
	type para struct{ label, text string }
	allergies := strings.TrimSpace(pet.Allergies)
	if allergies == "" {
		allergies = "не указаны"
	}
	paras := []para{{"Аллергии:", allergies}}
	if c := strings.TrimSpace(pet.Chronic); c != "" {
		paras = append(paras, para{"Хронические заболевания:", c})
	}

	textX := margin + 16 + 22 + 12
	textW := margin + contentW - 16 - textX
	type line struct {
		label, text string
	}
	var lines []line
	for _, pr := range paras {
		p.font(rubikBold, 13)
		lw := p.width(pr.label + " ")
		p.font(nunito, 13)
		for i, l := range p.wrap(pr.text, textW-lw, textW) {
			if i == 0 {
				lines = append(lines, line{pr.label, l})
			} else {
				lines = append(lines, line{"", l})
			}
		}
	}

	h := 24 + float64(len(lines))*lh
	p.gap(18)
	p.ensure(h)
	y0 := p.y
	p.fill(coral50)
	p.stroke(coral200)
	p.f.SetLineWidth(pt(1))
	p.rrect(margin, y0, contentW, h, 16, "FD")
	p.icon(icFlower, margin+16, y0+(h-22)/2, 22, 22, coral500)

	y := y0 + 12
	for _, l := range lines {
		x := textX
		if l.label != "" {
			p.font(rubikBold, 13)
			p.color(ink900)
			p.text(x, y, lh, l.label)
			x += p.width(l.label + " ")
		}
		p.font(nunito, 13)
		p.color(textBody)
		p.text(x, y, lh, l.text)
		y += lh
	}
	p.y = y0 + h
}

// ── Секции ────────────────────────────────────────────────────────────────────

const sectionHeadH = 28 + 12

// sectionHead рисует плашку с иконкой, заголовок капсом и линию до края.
// firstH — высота первого элемента секции: заголовок не должен остаться
// последней строкой страницы.
func (p *doc) sectionHead(glyph string, bg, fg rgb, title string, firstH float64) {
	p.gap(26)
	p.ensure(sectionHeadH + firstH)
	y := p.y
	p.fill(bg)
	p.rrect(margin, y, 28, 28, 9, "F")
	p.icon(glyph, margin, y, 28, 16, fg)

	p.font(rubikBold, 12)
	p.color(ink700)
	w := p.spaced(margin+38, y, 28, strings.ToUpper(title), 1.2)
	p.hline(margin+38+w+10, margin+contentW, y+14, violet100)
	p.y = y + sectionHeadH
}

func (p *doc) emptyNote(text string) {
	p.font(nunito, 13)
	p.color(textMuted)
	p.text(margin+10, p.y, 22, text)
	p.y += 22
}

// cardRow — строка списка в рамке (лечение, симптомы).
type cardRow struct {
	title, suffix, sub string
	badge              *badge
	subLines           []string
}

func (p *doc) layoutRows(rows []cardRow) []float64 {
	hs := make([]float64, len(rows))
	for i := range rows {
		r := &rows[i]
		textW := contentW - 32
		if r.badge != nil {
			textW -= p.badgeWidth(r.badge, 12) + 12
		}
		p.font(nunito, 12)
		if r.sub != "" {
			r.subLines = p.wrap(r.sub, textW, textW)
		}
		hs[i] = 24 + 18.2 + float64(len(r.subLines))*16.8
	}
	return hs
}

// cardList рисует строки в скруглённой рамке. Не поместившиеся строки уходят
// на следующую страницу в своей рамке.
func (p *doc) cardList(rows []cardRow, hs []float64) {
	for i := 0; i < len(rows); {
		p.ensure(hs[i])
		top := p.y
		j := i
		for y := top; j < len(rows) && y+hs[j] <= bottom; j++ {
			p.cardRow(rows[j], y, hs[j])
			y += hs[j]
			if j+1 < len(rows) && y+hs[j+1] <= bottom {
				p.hline(margin+16, margin+contentW-16, y, violet100)
			}
			p.y = y
		}
		p.stroke(violet100)
		p.f.SetLineWidth(pt(1))
		p.rrect(margin, top, contentW, p.y-top, 16, "D")
		i = j
	}
}

func (p *doc) cardRow(r cardRow, y, h float64) {
	x := margin + 16
	textW := contentW - 32
	if r.badge != nil {
		textW -= p.badgeWidth(r.badge, 12) + 12
		p.drawBadge(*r.badge, margin+contentW-16, y+h/2, 12)
	}

	suffix := ""
	if r.suffix != "" {
		suffix = " · " + r.suffix
	}
	p.font(nunitoSemi, 12)
	suffixW := p.width(suffix)

	p.font(rubikBold, 14)
	p.color(ink950)
	title := p.ellipsize(r.title, textW-suffixW)
	p.text(x, y+12, 18.2, title)
	if suffix != "" {
		tw := p.width(title)
		p.font(nunitoSemi, 12)
		p.color(textMuted)
		p.text(x+tw, y+12, 18.2, suffix)
	}

	p.font(nunito, 12)
	p.color(textMuted)
	for i, l := range r.subLines {
		p.text(x, y+12+18.2+float64(i)*16.8, 16.8, l)
	}
}

func (p *doc) pills(pills []Pill) {
	sorted := append([]Pill(nil), pills...)
	sort.SliceStable(sorted, func(i, j int) bool { return before(sorted[i].Start, sorted[j].Start) })

	today := startOfDay(p.now)
	rows := make([]cardRow, 0, len(sorted))
	for _, pl := range sorted {
		b := badgeActive
		if pl.Start != nil && pl.Start.After(today.Add(24*time.Hour-time.Nanosecond)) {
			b = badgePlanned
		}
		rows = append(rows, cardRow{title: pl.Name, sub: pillSubtitle(pl), badge: &b})
	}
	p.listSection(icPill, mint100, mint500, "Текущее лечение", rows, "Сейчас лечения нет")
}

func (p *doc) symptoms(symptoms []Symptom) {
	sorted := append([]Symptom(nil), symptoms...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Date.After(sorted[j].Date) })

	rows := make([]cardRow, 0, len(sorted))
	for _, s := range sorted {
		title := s.Title
		if title == "" {
			title = "Симптом"
		}
		rows = append(rows, cardRow{
			title:  title,
			suffix: shortDate(s.Date),
			sub:    s.Note,
			badge:  severityBadge(s.Severity),
		})
	}
	p.listSection(icHeart, coral50, coral500, "Симптомы за последние 30 дней", rows, "Симптомов не отмечено")
}

func (p *doc) listSection(glyph string, bg, fg rgb, title string, rows []cardRow, empty string) {
	if len(rows) == 0 {
		p.sectionHead(glyph, bg, fg, title, 22)
		p.emptyNote(empty)
		return
	}
	hs := p.layoutRows(rows)
	p.sectionHead(glyph, bg, fg, title, hs[0])
	p.cardList(rows, hs)
}

// ── Таблицы: прививки и обработки ────────────────────────────────────────────

type cell struct {
	text  string
	badge *badge
}

const (
	tableHeadH = 28.3
	tableRowH  = 36.2
)

// latestByName оставляет по каждому препарату последнюю запись: врачу нужен
// текущий статус, а не вся история ревакцинаций.
func latestByName(ts []Treatment, vaccines bool) []Treatment {
	sorted := append([]Treatment(nil), ts...)
	sort.SliceStable(sorted, func(i, j int) bool { return after(sorted[i].Date, sorted[j].Date) })
	seen := map[string]bool{}
	var out []Treatment
	for _, t := range sorted {
		if t.IsVaccine() != vaccines {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(t.Name)) + "\x00" + t.Kind
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, t)
	}
	return out
}

func (p *doc) vaccines(ts []Treatment) {
	today := startOfDay(p.now)
	var rows [][]cell
	for _, t := range latestByName(ts, true) {
		var b *badge
		if t.Next != nil {
			if t.Next.Before(today) {
				b = &badgeExpired
			} else {
				b = &badgeValid
			}
		}
		rows = append(rows, []cell{{text: nameOr(t.Name, "Прививка")}, {text: optDate(t.Date)}, {text: optDate(t.Next)}, {badge: b}})
	}
	p.table(icSyringe, violet100, violet600, "Прививки",
		[]string{"Вакцина", "Дата", "Повтор", "Статус"}, rows, "Прививок нет")
}

func (p *doc) treatments(ts []Treatment) {
	var rows [][]cell
	for _, t := range latestByName(ts, false) {
		rows = append(rows, []cell{{text: nameOr(t.Name, "Препарат")}, {text: treatmentTarget(t.Kind)}, {text: optDate(t.Date)}, {text: optDate(t.Next)}})
	}
	p.table(icShield, sky50, sky500, "Обработки",
		[]string{"Препарат", "От чего", "Дата", "Следующая"}, rows, "Обработок нет")
}

var tableCols = []float64{0.40, 0.20, 0.20, 0.20}

func (p *doc) table(glyph string, bg, fg rgb, title string, head []string, rows [][]cell, empty string) {
	if len(rows) == 0 {
		p.sectionHead(glyph, bg, fg, title, 22)
		p.emptyNote(empty)
		return
	}
	p.sectionHead(glyph, bg, fg, title, tableHeadH+tableRowH)
	p.tableHead(head)
	for _, r := range rows {
		if p.y+tableRowH > bottom {
			p.addPage()
			p.tableHead(head)
		}
		p.hline(margin, margin+contentW, p.y, violet100)
		x := margin
		for i, c := range r {
			w := contentW * tableCols[i]
			switch {
			case c.badge != nil:
				p.font(rubikBold, 11)
				bw := p.width(c.badge.text) + 20
				p.drawBadge(*c.badge, x+10+bw, p.y+tableRowH/2, 10)
			case i == 0:
				p.font(nunitoSemi, 13)
				p.color(ink900)
				p.text(x+10, p.y+9, 18.2, p.ellipsize(c.text, w-20))
			default:
				p.font(nunito, 13)
				p.color(textBody)
				p.text(x+10, p.y+9, 18.2, p.ellipsize(c.text, w-20))
			}
			x += w
		}
		p.y += tableRowH
	}
}

func (p *doc) tableHead(head []string) {
	x := margin
	p.font(rubikBold, 11)
	p.color(textMuted)
	for i, h := range head {
		p.spaced(x+10, p.y+6, 14.3, strings.ToUpper(h), 0.66)
		x += contentW * tableCols[i]
	}
	p.y += tableHeadH
}

// ── Динамика веса ─────────────────────────────────────────────────────────────

// ChartPoints — сколько последних взвешиваний на графике. Больше подписи
// начинают налезать друг на друга.
const ChartPoints = 6

func (p *doc) weights(ws []Weight) {
	const (
		boxW  = 150.0
		chW   = contentW - boxW - 14
		inner = chW - 32
		s     = inner / 400 // масштаб viewBox 400×140 из макета
		h     = 14 + 140*s + 8
	)
	if len(ws) > ChartPoints {
		ws = ws[len(ws)-ChartPoints:]
	}
	p.sectionHead(icScales, violet100, violet600, "Динамика веса", h)
	y0 := p.y

	p.stroke(violet100)
	p.f.SetLineWidth(pt(1))
	p.rrect(margin, y0, chW, h, 16, "D")
	ox, oy := margin+16, y0+14
	at := func(x, y float64) (float64, float64) { return ox + x*s, oy + y*s }

	xs := chartXs(ws)
	ys := chartYs(ws)
	pts := make([]gofpdf.PointType, len(ws))
	for i := range ws {
		x, y := at(xs[i], ys[i])
		pts[i] = gofpdf.PointType{X: pt(x), Y: pt(y)}
	}

	bx1, by := at(20, 112)
	bx2, _ := at(380, 112)
	p.hline(bx1, bx2, by, violet100)

	// Заливка под линией.
	area := append([]gofpdf.PointType(nil), pts...)
	lastX, _ := at(xs[len(xs)-1], 112)
	firstX, _ := at(xs[0], 112)
	area = append(area, gofpdf.PointType{X: pt(lastX), Y: pt(by)}, gofpdf.PointType{X: pt(firstX), Y: pt(by)})
	p.fill(violet100)
	p.f.SetAlpha(0.6, "Normal")
	p.f.Polygon(area, "F")
	p.f.SetAlpha(1, "Normal")

	// Линия.
	p.stroke(violet500)
	p.f.SetLineWidth(pt(2.5 * s))
	p.f.SetLineCapStyle("round")
	p.f.SetLineJoinStyle("round")
	p.f.MoveTo(pts[0].X, pts[0].Y)
	for _, q := range pts[1:] {
		p.f.LineTo(q.X, q.Y)
	}
	p.f.DrawPath("D")
	p.f.SetLineCapStyle("butt")
	p.f.SetLineJoinStyle("miter")

	// Точки и подписи.
	last := len(ws) - 1
	for i, w := range ws {
		x, y := at(xs[i], ys[i])
		p.f.SetLineWidth(pt(2 * s))
		if i == last {
			p.fill(violet600)
			p.stroke(white)
			p.f.Circle(pt(x), pt(y), pt(4.5*s), "FD")
		} else {
			p.fill(white)
			p.stroke(violet500)
			p.f.Circle(pt(x), pt(y), pt(3.5*s), "FD")
		}

		label := fmt.Sprintf("%.1f", w.Kg)
		if i == last {
			p.font(rubikBlack, 12)
			p.color(violet700)
		} else {
			p.font(nunitoBold, 11)
			p.color(ink600)
		}
		_, ly := at(0, ys[i]-14)
		p.anchored(x, ly, label, i, last)

		p.font(nunito, 10)
		p.color(ink300)
		_, dy := at(0, 130)
		p.anchored(x, dy, w.Date.Format("02.01"), i, last)
	}

	// Плашка «Текущий вес».
	bx := margin + chW + 14
	p.fill(violet50)
	p.stroke(violet200)
	p.f.SetLineWidth(pt(1))
	p.rrect(bx, y0, boxW, h, 16, "FD")
	cur := ws[last]
	caption := shortDate(cur.Date)
	if t := weightTrend(ws); t != "" {
		caption += " · " + t
	}
	ty := y0 + (h-68.8)/2
	p.font(rubikBold, 11)
	p.color(textMuted)
	label := "ТЕКУЩИЙ ВЕС"
	p.spaced(bx+(boxW-p.spacedWidth(label, 0.66))/2, ty, 15.4, label, 0.66)
	p.font(rubikBlack, 24)
	p.color(violet700)
	v := weightText(cur.Kg)
	p.text(bx+(boxW-p.width(v))/2, ty+19.4, 30, v)
	p.font(nunito, 11)
	p.color(textMuted)
	caption = p.ellipsize(caption, boxW-24)
	p.text(bx+(boxW-p.width(caption))/2, ty+53.4, 15.4, caption)

	p.y = y0 + h
}

// anchored ставит подпись точки как text-anchor в SVG макета: первая — от
// начала, последняя — до конца, остальные по центру. y — базовая линия.
func (p *doc) anchored(x, baseline float64, s string, i, last int) {
	w := p.width(s)
	switch i {
	case 0:
	case last:
		x -= w
	default:
		x -= w / 2
	}
	p.f.Text(pt(x), pt(baseline), s)
}

// chartXs раскладывает точки по оси X пропорционально датам, как в макете.
// Если две точки оказались слишком близко (взвешивали дважды за неделю),
// подписи слипнутся — тогда шаг делаем равномерным.
func chartXs(ws []Weight) []float64 {
	const x0, x1, minGap = 20.0, 370.0, 40.0
	n := len(ws)
	xs := make([]float64, n)
	span := ws[n-1].Date.Sub(ws[0].Date).Hours()
	even := span <= 0
	if !even {
		for i, w := range ws {
			xs[i] = x0 + (x1-x0)*w.Date.Sub(ws[0].Date).Hours()/span
			if i > 0 && xs[i]-xs[i-1] < minGap {
				even = true
			}
		}
	}
	if even {
		for i := range xs {
			xs[i] = x0 + (x1-x0)*float64(i)/float64(n-1)
		}
	}
	return xs
}

func chartYs(ws []Weight) []float64 {
	const yLow, yHigh = 88.0, 36.8
	lo, hi := ws[0].Kg, ws[0].Kg
	for _, w := range ws {
		lo, hi = math.Min(lo, w.Kg), math.Max(hi, w.Kg)
	}
	ys := make([]float64, len(ws))
	for i, w := range ws {
		if hi-lo < 1e-9 {
			ys[i] = (yLow + yHigh) / 2
			continue
		}
		ys[i] = yLow - (w.Kg-lo)/(hi-lo)*(yLow-yHigh)
	}
	return ys
}

// ── Заметки врача ─────────────────────────────────────────────────────────────

func (p *doc) vetNotes() {
	const h = 16 + 16 + 1 + 3*23 + 16
	p.sectionHead(icNote, mint100, mint500, "Заметки врача", h)
	y0 := p.y
	p.stroke(violet200)
	p.f.SetLineWidth(pt(1))
	p.f.SetDashPattern([]float64{pt(4), pt(3)}, 0)
	p.rrect(margin, y0, contentW, h, 16, "D")
	p.f.SetDashPattern([]float64{}, 0)
	for i := 0; i < 4; i++ {
		p.hline(margin+16, margin+contentW-16, y0+32+float64(i)*23, violet100)
	}
	p.y = y0 + h
}

// ── Мелочи ────────────────────────────────────────────────────────────────────

// before упорядочивает даты, ставя пустые в конец.
func before(a, b *time.Time) bool {
	switch {
	case a == nil:
		return false
	case b == nil:
		return true
	}
	return a.Before(*b)
}

// after — обратный порядок, пустые даты тоже в конце.
func after(a, b *time.Time) bool {
	switch {
	case a == nil:
		return false
	case b == nil:
		return true
	}
	return a.After(*b)
}

func nameOr(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}
