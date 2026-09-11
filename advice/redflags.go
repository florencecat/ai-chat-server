package advice

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// RedFlags — маркеры состояний, при которых ответ не может быть спокойнее
// «к врачу в течение суток».
//
// Проверка односторонняя: уровень только поднимается. Понижать автоматически
// нельзя ни при каких условиях — цена ошибки несимметрична.
type RedFlags struct {
	// markers — нормализованные (нижний регистр) подстроки.
	markers []string
}

type redFlagsFile struct {
	Markers []string `yaml:"markers"`
}

// LoadRedFlags читает YAML вида {markers: [...]}. Отсутствие файла — не
// ошибка: сервер работает и без эскалации, просто без страховки.
func LoadRedFlags(path string) (*RedFlags, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &RedFlags{}, nil
		}
		return nil, fmt.Errorf("advice: read red flags: %w", err)
	}
	var f redFlagsFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("advice: parse red flags: %w", err)
	}
	rf := &RedFlags{}
	for _, m := range f.Markers {
		if m = strings.ToLower(strings.TrimSpace(m)); m != "" {
			rf.markers = append(rf.markers, m)
		}
	}
	return rf, nil
}

// Len — сколько маркеров загружено (для стартового лога).
func (rf *RedFlags) Len() int {
	if rf == nil {
		return 0
	}
	return len(rf.markers)
}

// Match возвращает маркеры, найденные в тексте.
func (rf *RedFlags) Match(text string) []string {
	if rf == nil || len(rf.markers) == 0 {
		return nil
	}
	lower := strings.ToLower(text)
	var hits []string
	for _, m := range rf.markers {
		if strings.Contains(lower, m) {
			hits = append(hits, m)
		}
	}
	return hits
}

// Escalate поднимает urgency до vet_24h, если в тексте ответа есть маркер, а
// заявленный уровень ниже. Возвращает найденные маркеры и признак того, что
// уровень был изменён, — расхождение модели с правилом стоит залогировать.
func (rf *RedFlags) Escalate(a *Answer) (hits []string, raised bool) {
	hits = rf.Match(a.Response)
	if len(hits) == 0 {
		return nil, false
	}
	// Пустой urgency тоже поднимаем: маркер найден, метаданных нет — значит
	// клиенту нечего показать поверх текста, а показать нужно.
	if urgencyRank(a.Urgency) >= urgencyRank(UrgencyVet24h) {
		return hits, false
	}
	a.Urgency = UrgencyVet24h
	return hits, true
}
