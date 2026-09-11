package advice

import (
	"regexp"
	"strings"
)

// Разметка разрешена только в поле response. В остальные поля она всё равно
// протекает: промт велит выделять названия препаратов жирным, и модель
// выделяет их и в "response", и в name события — а клиент рисует name
// обычным текстом, так что пользователь видит «приём **Пульмикорта**».
//
// Промт эту утечку уменьшает, но гарантией быть не может, поэтому маркеры
// снимаются на сервере.
var (
	// Парные маркеры: **жирный**, __жирный__, ~~зачёркнутый~~, *курсив*,
	// `моноширинный`. Внутреннюю группу оставляем, обёртку убираем.
	pairedMarkupRe = regexp.MustCompile("(\\*\\*|__|~~)(.+?)(\\*\\*|__|~~)|\\*([^*\n]+?)\\*|`([^`\n]+?)`")
	// Заголовки и маркеры списка в начале строки.
	leadingMarkupRe = regexp.MustCompile(`(?m)^\s*(#{1,6}\s+|[-*+]\s+|\d+\.\s+|>\s+)`)
	// Ссылка [текст](url) — оставляем текст.
	linkRe = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
)

// plainText снимает markdown-разметку с поля, которое клиент рисует как
// обычный текст. Оставляет содержимое, убирает только маркеры.
func plainText(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	s = linkRe.ReplaceAllString(s, "$1")
	s = leadingMarkupRe.ReplaceAllString(s, "")
	// Несколько проходов: **жирный с `кодом` внутри** снимается послойно.
	for i := 0; i < 3; i++ {
		next := pairedMarkupRe.ReplaceAllString(s, "$2$4$5")
		if next == s {
			break
		}
		s = next
	}
	// Непарные остатки вроде «доза 5** мг» — маркеры, а не содержание.
	s = strings.NewReplacer("**", "", "__", "", "~~", "", "`", "").Replace(s)
	return strings.TrimSpace(s)
}

// plainStrings применяет plainText к списку, отбрасывая опустевшие элементы.
func plainStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := in[:0]
	for _, s := range in {
		if s = plainText(s); s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
