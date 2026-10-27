package handlers

import (
	"sync"
	"time"
)

// reportLimiter — антиспам для /report. Два независимых ограничения:
//
//   - запросы в минуту — любые, включая отдачу из кэша: каждый запрос стоит
//     пяти лёгких походов в PocketBase за отпечатком;
//   - генерации — только реальная сборка PDF по новым данным: пауза между
//     ними и дневной потолок.
//
// Счётчики в памяти: сервер работает в одном экземпляре, а после рестарта
// пользователь в худшем случае получит одну лишнюю генерацию.
type reportLimiter struct {
	perMinute int
	cooldown  time.Duration
	perDay    int

	mu        sync.Mutex
	users     map[string]*reportUsage
	lastPrune time.Time
}

type reportUsage struct {
	windowStart time.Time
	requests    int

	lastGen time.Time
	day     string
	gens    int
}

func newReportLimiter(perMinute int, cooldown time.Duration, perDay int) *reportLimiter {
	return &reportLimiter{
		perMinute: perMinute,
		cooldown:  cooldown,
		perDay:    perDay,
		users:     map[string]*reportUsage{},
	}
}

func (l *reportLimiter) usage(userID string, now time.Time) *reportUsage {
	// Раз в час выбрасываем тех, кто не появлялся сутки, чтобы карта не росла.
	if now.Sub(l.lastPrune) > time.Hour {
		for id, u := range l.users {
			if now.Sub(u.windowStart) > 24*time.Hour && now.Sub(u.lastGen) > 24*time.Hour {
				delete(l.users, id)
			}
		}
		l.lastPrune = now
	}
	u, ok := l.users[userID]
	if !ok {
		u = &reportUsage{}
		l.users[userID] = u
	}
	return u
}

// allowRequest учитывает запрос в минутном окне. Возвращает, сколько ждать,
// если лимит исчерпан.
func (l *reportLimiter) allowRequest(userID string, now time.Time) (bool, time.Duration) {
	if l.perMinute <= 0 {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	u := l.usage(userID, now)
	if now.Sub(u.windowStart) >= time.Minute {
		u.windowStart, u.requests = now, 0
	}
	if u.requests >= l.perMinute {
		return false, u.windowStart.Add(time.Minute).Sub(now)
	}
	u.requests++
	return true, 0
}

// allowGenerate проверяет, можно ли собрать новый PDF, ничего не списывая:
// генерация может не состояться (PocketBase недоступен), и тогда попытка не
// должна сгорать.
func (l *reportLimiter) allowGenerate(userID string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	u := l.usage(userID, now)
	if wait := u.lastGen.Add(l.cooldown).Sub(now); wait > 0 {
		return false, wait
	}
	if l.perDay > 0 && u.day == dayKey(now) && u.gens >= l.perDay {
		y, m, d := now.UTC().Date()
		return false, time.Date(y, m, d+1, 0, 0, 0, 0, time.UTC).Sub(now)
	}
	return true, 0
}

func (l *reportLimiter) recordGenerate(userID string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	u := l.usage(userID, now)
	u.lastGen = now
	if d := dayKey(now); u.day != d {
		u.day, u.gens = d, 0
	}
	u.gens++
}

func dayKey(t time.Time) string { return t.UTC().Format("2006-01-02") }

// keyedMutex сериализует сборку отчёта одного питомца: пять одновременных
// нажатий «Поделиться» дают одну генерацию, остальные получают её из
// pdf_reports. Заодно исключает гонку двух create одной записи.
type keyedMutex struct {
	m sync.Map // string → *sync.Mutex
}

func (k *keyedMutex) lock(key string) func() {
	v, _ := k.m.LoadOrStore(key, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}
