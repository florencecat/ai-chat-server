# ai-chat-server 

[![Build & Deploy](https://github.com/florencecat/ai-chat-server/actions/workflows/deploy.yml/badge.svg)](https://github.com/florencecat/ai-chat-server/actions/workflows/deploy.yml)

HTTP-сервис на Go, который проксирует запросы к [Yandex AI Studio](https://aistudio.yandex.ru/). Принимает сообщения от клиента, проверяет аутентификацию и квоты пользователя через [PocketBase](https://pocketbase.io/), собирает системную инструкцию, валидирует ответ модели по строгой схеме, кеширует его и отдаёт клиенту.

Провайдер GigaChat оставлен рабочим, но не развивается: строгой схемы ответа у него нет, формат держится только на промте.

## Возможности

- **Проксирование к Yandex AI Studio** через OpenAI-совместимый `/v1/chat/completions`: история диалога массивом сообщений, ограничение длины ответа, строгая схема через `response_format.json_schema`.
- **Серверная системная инструкция** — промт и формат ответа живут в `prompts/`, а не в APK, поэтому меняются без релиза приложения.
- **Строгий контракт ответа** — схема уходит модели, а результат всё равно нормализуется на сервере: чужие категории событий, незнакомые трекеры и записи без ключевого значения отбрасываются, а не исчезают молча на клиенте.
- **Красные флаги** — если в тексте ответа есть маркер из `prompts/red-flags.yaml`, а `urgency` занижен, сервер поднимает уровень. Понижать автоматически он не умеет.
- **Аутентификация через PocketBase** — клиент присылает свой JWT, сервер верифицирует его и находит связанную запись пользователя.
- **Квоты** — лимиты «N запросов в минуту» и «N запросов в день» на пользователя, счётчики хранятся в PocketBase.
- **Кеширование** ответов в BoltDB с TTL. Ключ — последнее сообщение пользователя, питомец, модель и версия промта, а не всё тело запроса.
- **Санитизация ввода** — обрезка управляющих символов и ограничение длины каждого сообщения.
- **Graceful shutdown** по `SIGINT`/`SIGTERM`.

## Архитектура

```
Flutter / клиент
      │  Authorization: Bearer <PB JWT>
      │  { "message": "..." }
      ▼
┌─────────────────────────────────────────┐
│              ai-chat-server              │
│                                          │
│  1. Верификация JWT      ──────► PocketBase (auth-refresh)
│  2. Поиск/создание tokens ─────► PocketBase (по profile = user.id)
│  3. Проверка квоты                       │
│  4. Кеш (BoltDB) ─── hit ──► ответ       │
│  5. Сборка промта (prompts/)             │
│  6. Запрос к модели      ──────► Yandex AI Studio
│  7. Разбор и нормализация ответа         │
│  8. Красные флаги: поднять urgency       │
│  9. Списание квоты       ──────► PocketBase
│                                          │
└─────────────────────────────────────────┘
```

Невалидный JSON от модели — не ответ: сервер один раз повторяет запрос и,
если второй ответ тоже не разбирается, отдаёт `LLM_ERROR`. В кэш попадает
уже нормализованный ответ.

Структура пакетов:

| Пакет        | Назначение                                          |
|--------------|-----------------------------------------------------|
| `config`      | Загрузка конфигурации из переменных окружения                    |
| `llm`         | Абстракция LLM-провайдера (`yandex` / `gigachat`)                |
| `advice`      | Контракт ответа: JSON-схема, нормализация, красные флаги         |
| `gigachat`    | Клиент GigaChat: OAuth-токен, чат-запросы (не развивается)       |
| `pocketbase`  | Клиент PocketBase: верификация пользователя, квоты, права доступа |
| `rustore`     | Клиент RuStore Public API и расшифровка серверных уведомлений    |
| `entitlement` | Доменная модель прав доступа: тарифы, верификация, вебхуки       |
| `cache`       | Кеш ответов поверх BoltDB с TTL                                  |
| `handlers`    | HTTP-обработчики (Gin)                                           |

## Премиум-статус (RuStore Pay SDK)

Источник истины по правам — этот бэкенд, а не клиент. Клиент после успешной
оплаты через Pay SDK отдаёт серверу только `purchaseId`; всё остальное сервер
выясняет сам у RuStore.

```
Flutter (Pay SDK)                ai-server                     RuStore public-api
      │  POST /verify {purchaseId}    │                                 │
      ├──────────────────────────────►│  GET /public/v4/subscription/…  │
      │                               ├────────────────────────────────►│
      │                               │  paymentState=1 → права в PB    │
      │                               │  POST …:acknowledge (доставка)  │
      │                               ├────────────────────────────────►│
      │◄──────────────────────────────┤  Entitlement                    │
      │                               │                                 │
      │  GET /entitlement             │◄── POST /rustore/webhook ───────┤
      │◄─────────────────────────────►│    (продление / отмена / рефанд)│
```

Тариф решает всё на сервере: `/chat` по правам пользователя выбирает модель
(`MODEL_FREE` / `MODEL_PREMIUM`) и дневной лимит. Клиенту ничего решать не нужно.

Ключевые свойства реализации:

- **Одна покупка — один аккаунт.** `purchaseId` привязывается к пользователю
  при первой верификации; попытка предъявить чужой — `409 PURCHASE_ALREADY_CLAIMED`.
- **Подлинность вебхука** обеспечивается расшифровкой AES-256-GCM ключом из
  консоли RuStore: подделать уведомление без ключа нельзя.
- **Grace-период** (`ENTITLEMENT_GRACE`, по умолчанию сутки) плюс ленивое
  обновление в `GET /entitlement` не дают потерять премиум из-за потерянного
  уведомления о продлении.
- **Деградация вниз, а не в 500.** Если PocketBase или RuStore недоступны,
  пользователь получает бесплатный тариф, а не ошибку.
- Кеш ответов разделён по модели — ответ улучшенной модели не попадёт
  бесплатному пользователю.

## API

Все запросы к `/chat`, `/quota`, `/entitlement` и `/verify` требуют заголовок
`Authorization: Bearer <PocketBase JWT>`. Вебхук RuStore авторизуется
шифрованием полезной нагрузки и заголовка не требует.

### `POST /chat` (v1)

Контракт запроса заморожен: его используют установленные сборки, которые шлют
весь диалог одной строкой.

```jsonc
// Request
{ "message": "Привет!" }
```

```jsonc
// 200 OK
{
  "response": { /* см. «Схема ответа» ниже */ },
  "cached": false,
  "tier": "premium",
  "prompt_version": "v1",
  "quota": {
    "requests_today": 3,
    "limit_day": 200,
    "limit_minute": 5
  }
}
```

Разметка ответа здесь по умолчанию выключена (`MARKUP_ENABLED=false`): сборки
без markdown-рендера показали бы `**звёздочки**` текстом. Включается
переменной окружения, без релиза приложения.

### `POST /v2/chat`

То же самое, но диалог приходит массивом, системную инструкцию собирает
сервер, а в `datetime` уезжает местное время пользователя.

```jsonc
// Request
{
  "messages": [
    { "role": "user",      "content": "Кот чихает третий день" },
    { "role": "assistant", "content": "…" },
    { "role": "user",      "content": "Температуры нет" }
  ],
  "pet_id":      "rec123",                     // для ключа кэша
  "pet_context": "Барсик, кот, 3 года, 4.2 кг…", // склеивается в промт сервером
  "client_time": "2026-09-11T14:32:00+03:00",
  "tz":          "Europe/Moscow"
}
```

Все поля кроме `messages` необязательны. Роль `system` от клиента
игнорируется: формат ответа задаёт сервер. Без `client_time` / `tz` берётся
время сервера — поведение прежнее.

Ответ — то же тело, что у v1, с `"prompt_version"` и всегда включённой
разметкой.

### Схема ответа

Поле `response` — объект фиксированной формы. Схема уходит модели
(`response_format.json_schema`) и повторно проверяется на сервере: всё, что
клиент не умеет разобрать, отбрасывается до отдачи и до записи в кэш.

```jsonc
{
  "response": "Текст ответа. Обязателен: ответ без текста — это LLM_ERROR.",

  "events": [                       // не больше 3
    {
      "name":     "Дать таблетку",
      "category": "health",         // health | grooming | other
      "datetime": "2026-09-12T09:00:00",  // местное время, без «Z» и смещения
      "repeat":   "daily"           // none | daily | weekly | monthly
    }
  ],

  "entries": [                      // не больше 5
    { "tracker": "symptom", "symptom": "sneezing", "severity": "mild",
      "datetime": "2026-09-11T14:00:00" }
  ],

  "urgency":   "monitor",           // self_care | monitor | vet_24h | emergency
  "red_flags": ["кровь в выделениях", "отказ от воды дольше суток"],
  "follow_up_questions": ["Есть ли выделения из глаз?"]   // не больше 3
}
```

Схема строгая в смысле OpenAI strict mode: Yandex отвергает её целиком, если
хоть одно свойство не перечислено в `required`. Поэтому модель обязана
присылать все поля верхнего уровня, и `urgency` в ответе есть всегда —
различать «метаданных нет» по его отсутствию нельзя. Пустые `events`,
`entries`, `red_flags` и `follow_up_questions` из тела ответа вырезаются.

По той же причине модель присылает у каждой записи `entries` все поля сразу,
включая чужие для её трекера (в перечисления добавлена пустая строка как
легальное «не моё»). Клиенту они не уезжают: запись пересобирается из полей
своего трекера в `Entry.normalize`.

`maxItems` в схеме нет — strict mode его не поддерживает. Лимиты «не больше
трёх событий, пяти записей» держатся на промте и гарантируются
нормализацией.

Обязательные поля `entries` по трекерам:

| `tracker` | Обязательное поле          | Остальные поля                                                    |
|-----------|----------------------------|-------------------------------------------------------------------|
| `mood`    | `mood`                     | `happy` / `calm` / `sick` / `playful`                              |
| `weight`  | `weight_kg` > 0            | килограммы                                                         |
| `symptom` | `symptom`                  | `severity`: `mild` / `moderate` / `severe` (по умолчанию `mild`), `note` |
| `walk`    | `minutes` > 0              | `activities`: `active` / `calm` / `dogGames` / `training`           |
| `meal`    | `grams` > 0 **или** `food` | `appetite` 1–5, `kind`: `natural` / `dry` / `wet` / `treat`         |
| `note`    | `text`                     | —                                                                   |

Допустимые `symptom`: `vomiting`, `diarrhea`, `refused_food`, `lethargy`,
`sneezing`, `coughing`, `scratching`, `limping`.

Предложение без обязательного поля отбрасывается целиком — по такой карточке
всё равно ничего не записать.

#### Красные флаги

Если в тексте ответа встречается маркер из `prompts/red-flags.yaml`, а
`urgency` ниже `vet_24h`, сервер поднимает уровень и пишет в лог расхождение.
Понижения нет ни при каких условиях: цена ошибки несимметрична. Маркеры
ищутся подстрокой в нижнем регистре, поэтому в файле пишется корень слова
(`судорог` ловит и «судороги», и «судорогами»).

### `GET /quota`

Возвращает текущее состояние квоты пользователя (тело как в поле `quota` выше).
Лимиты — по действующему тарифу.

### `GET /entitlement`

Текущие права пользователя — серверная сторона `EntitlementApi.fetch()`.
Если срок подписки истекает или уже истёк, данные подтягиваются из RuStore.

```jsonc
// 200 OK
{
  "tier": "premium",              // "free" | "premium"
  "active": true,
  "product_id": "premium_month",
  "purchase_id": "3aa0c7bd-964e-4562-b218-fe365adb4ae3",
  "order_id": "…",
  "state": "ACTIVE",              // ACTIVE | PAUSED | CLOSED | TERMINATED
  "period": "MAIN",               // TRIAL | PROMO | MAIN | GRACE | HOLD
  "expires_at": "2026-09-10T12:00:00Z",
  "auto_renewing": true,
  "source": "rustore",            // "rustore" | "manual"
  "updated_at": "2026-08-10T12:00:00Z",
  "limits": { "requests_per_day": 200, "requests_per_minute": 5 }
}
```

Бесплатный тариф отдаётся тем же телом с `"tier": "free"`, `"active": false`
и лимитами free-плана. Ошибок при отсутствии прав нет — только `200`.

### `POST /verify`

Верификация покупки — серверная сторона `EntitlementApi.verify(purchaseId)`.
Сервер запрашивает Subscription Data v4, при оплаченной подписке записывает
права и подтверждает доставку (Confirm Delivery v2).

```jsonc
// Request (принимается и purchaseId, и purchase_id)
{ "purchaseId": "3aa0c7bd-964e-4562-b218-fe365adb4ae3" }
```

Ответ — то же тело, что у `GET /entitlement`.

### `POST /rustore/webhook`

Серверные уведомления RuStore о жизненном цикле подписки. Путь настраивается
через `RUSTORE_WEBHOOK_PATH`. Обрабатываются `SUBSCRIPTION_EVENT`
(`ACTIVATED`, `RENEWED`, `RESUMED`, `CANCELLED`, `PAYMENT_FAILED`, `CLOSED`) и
`INVOICE_STATUS` (возвраты и отмены платежа).

Отвечает `200` сразу после расшифровки — RuStore ждёт ответ за 3 секунды, иначе
шлёт до 16 повторов за 36 часов. Применение прав, которое ходит в public-api,
выполняется в фоне.

### `GET /health`

```json
{ "status": "ok" }
```

### Коды ошибок

| HTTP | `code`                     | Когда                                          |
|------|----------------------------|------------------------------------------------|
| 400  | `INVALID_REQUEST`          | Невалидное тело запроса                        |
| 400  | `EMPTY_MESSAGE`            | Сообщение пустое после санитизации             |
| 400  | `INVALID_PAYLOAD`          | Вебхук: payload не расшифровался               |
| 401  | `MISSING_AUTH`             | Нет заголовка `Authorization`                  |
| 401  | `UNAUTHORIZED`             | Невалидный или истёкший JWT                    |
| 403  | `TOKEN_NOT_FOUND`          | Записи в `tokens` нет и её не удалось создать  |
| 404  | `PURCHASE_NOT_FOUND`       | RuStore не знает такой `purchaseId`            |
| 409  | `PURCHASE_NOT_CONFIRMED`   | Покупка найдена, но не оплачена                |
| 409  | `PURCHASE_ALREADY_CLAIMED` | `purchaseId` уже привязан к другому аккаунту   |
| 429  | `RATE_LIMIT_MINUTE`        | Превышен лимит запросов в минуту               |
| 429  | `RATE_LIMIT_DAY`           | Превышена дневная квота                        |
| 502  | `VERIFICATION_FAILED`      | RuStore public-api недоступен или ответил ошибкой |
| 503  | `VERIFICATION_DISABLED`    | `RUSTORE_ENABLED=false`                        |
| 503  | `WEBHOOK_DISABLED`         | Не задан `RUSTORE_NOTIFICATION_KEY`            |
| 503  | `UPSTREAM_RATE_LIMIT`      | Провайдер модели вернул `429`                  |
| 500  | `LLM_ERROR`                | Ошибка обращения к модели                      |

## Конфигурация

Все настройки задаются через переменные окружения (можно через `.env` — см. [`.env.example`](.env.example)).

| Переменная             | По умолчанию                  | Описание                                            |
|------------------------|-------------------------------|-----------------------------------------------------|
| `PORT`                 | `8080`                        | Порт HTTP-сервера                                   |
| `LLM_PROVIDER`         | `yandex`                      | `yandex` \| `gigachat`                              |
| `YANDEX_FOLDER_ID`     | —                             | Каталог Yandex Cloud                                |
| `YANDEX_API_KEY`       | —                             | API-ключ сервисного аккаунта                        |
| `YANDEX_MODEL`         | `yandexgpt-lite/latest`       | Модель по умолчанию; принимается и готовый `gpt://…` |
| `YANDEX_TEMPERATURE`   | `0.25`                        | Температура                                         |
| `YANDEX_MAX_TOKENS`    | `1500`                        | Потолок длины ответа (аварийный: длину держит промт) |
| `YANDEX_STRUCTURED_OUTPUT` | `true`                    | Слать `response_format.json_schema`                 |
| `SYSTEM_PROMPT_FILE`   | `prompts/system.txt`          | Промт для `/chat`                                   |
| `SYSTEM_PROMPT`        | _(см. код)_                   | Промт строкой, если файла нет                       |
| `SYSTEM_PROMPT_V2_FILE`| `prompts/system_v2.txt`       | Промт для `/v2/chat`                                |
| `FORMAT_MARKUP_FILE`   | `prompts/format_markup.txt`   | Блок `{{FORMAT}}` с разметкой                       |
| `FORMAT_PLAIN_FILE`    | `prompts/format_plain.txt`    | Блок `{{FORMAT}}` без разметки                      |
| `MARKUP_ENABLED`       | `false`                       | Разметка в ответах `/chat`; в `/v2/chat` включена всегда |
| `PROMPT_VERSION`       | `v1`                          | Входит в ключ кэша и в тело ответа                  |
| `RED_FLAGS_FILE`       | `prompts/red-flags.yaml`      | Маркеры, поднимающие `urgency`                      |
| `MAX_MESSAGE_LEN`      | `4000`                        | Максимальная длина одного сообщения                 |
| `MAX_HISTORY_MESSAGES` | `10`                          | Сколько сообщений истории уходит модели             |
| `MAX_HISTORY_CHARS`    | `8000`                        | Суммарный потолок истории в символах                |
| `GIGACHAT_AUTH_KEY`    | —                             | Готовая Base64-строка «Авторизационные данные»      |
| `GIGACHAT_CLIENT_ID`   | —                             | Альтернатива `AUTH_KEY`: client id                  |
| `GIGACHAT_CLIENT_SECRET`| —                            | Альтернатива `AUTH_KEY`: client secret              |
| `GIGACHAT_SCOPE`       | `GIGACHAT_API_PERS`           | Scope доступа                                       |
| `GIGACHAT_MODEL`       | `GigaChat`                    | Модель                                              |
| `GIGACHAT_SKIP_TLS`    | `true`                        | Пропускать проверку TLS (самоподписанные сертификаты Сбера) |
| `CACHE_TTL`            | `1h`                          | Время жизни кеша                                    |
| `DB_PATH`              | `data/ai-server.db`           | Путь к файлу BoltDB                                 |
| `QUOTA_PER_MINUTE`     | `1`                           | Лимит запросов в минуту (free)                      |
| `QUOTA_PER_DAY`        | `15`                          | Дневной лимит запросов (free)                       |
| `QUOTA_PER_MINUTE_PREMIUM` | `QUOTA_PER_MINUTE`        | Лимит запросов в минуту (premium)                   |
| `QUOTA_PER_DAY_PREMIUM`| `200`                         | Дневной лимит запросов (premium)                    |
| `MODEL_FREE`           | —                             | Базовая модель; пусто — модель провайдера по умолчанию |
| `MODEL_PREMIUM`        | —                             | Улучшенная модель для премиум-пользователей          |
| `ENTITLEMENT_GRACE`    | `24h`                         | Сколько премиум живёт после `expires_at`             |
| `PB_URL`               | `http://127.0.0.1:8090`       | URL PocketBase                                      |
| `PB_ADMIN_EMAIL`       | —                             | Email суперпользователя PocketBase                  |
| `PB_ADMIN_PASSWORD`    | —                             | Пароль суперпользователя PocketBase                 |

> При `LLM_PROVIDER=yandex` обязательны `YANDEX_FOLDER_ID` и `YANDEX_API_KEY`.
> При `LLM_PROVIDER=gigachat` нужно задать **либо** `GIGACHAT_AUTH_KEY`, **либо** пару `GIGACHAT_CLIENT_ID` + `GIGACHAT_CLIENT_SECRET`.

> `PROMPT_VERSION` меняйте вместе с любой правкой файлов в `prompts/`: она
> входит в ключ кэша, иначе после деплоя пользователи ещё `CACHE_TTL`
> получают ответы по старому промту.

> `GIGACHAT_SKIP_TLS` выключает проверку сертификатов для **всего**
> HTTP-клиента GigaChat. На Yandex-провайдера, PocketBase и RuStore она не
> влияет — у них свои клиенты с обычной проверкой.

### RuStore

| Переменная                 | По умолчанию                   | Описание                                             |
|----------------------------|--------------------------------|------------------------------------------------------|
| `RUSTORE_ENABLED`          | `false`                        | Включает верификацию покупок                         |
| `RUSTORE_API_URL`          | `https://public-api.rustore.ru`| Хост public-api                                      |
| `RUSTORE_KEY_ID`           | —                              | `keyId` ключа сервисного доступа из консоли          |
| `RUSTORE_PRIVATE_KEY_FILE` | —                              | Путь к приватному RSA-ключу (PEM)                    |
| `RUSTORE_PRIVATE_KEY`      | —                              | Альтернатива файлу: PEM или base64(PEM) одной строкой |
| `RUSTORE_PACKAGE_NAME`     | —                              | Package name приложения                              |
| `RUSTORE_APP_ID`           | —                              | ID приложения (нужен только для разовых покупок)     |
| `RUSTORE_SUBSCRIPTION_IDS` | —                              | Коды подписок через запятую                          |
| `RUSTORE_SANDBOX`          | `false`                        | Работа с песочницей                                  |
| `RUSTORE_NOTIFICATION_KEY` | —                              | Ключ расшифровки серверных уведомлений (AES-256)     |
| `RUSTORE_WEBHOOK_PATH`     | `/rustore/webhook`             | Путь вебхука                                         |

> Клиентский `EntitlementApi.verify` присылает только `purchaseId`, а
> Subscription Data v4 требует ещё и код продукта — поэтому сервер перебирает
> `RUSTORE_SUBSCRIPTION_IDS`. Новый тариф в консоли надо добавить и сюда.

Ключ доступа заводится в консоли RuStore (Настройки → Ключи доступа), ключ
расшифровки уведомлений — в разделе Монетизация → Серверные уведомления,
там же указывается `https://…{RUSTORE_WEBHOOK_PATH}` и тип уведомлений
(платежи + подписки).

## PocketBase

Сервис рассчитан на три коллекции:

- **`users`** (тип `auth`) — пользователи, аутентификация по email/паролю.
- **`tokens`** (тип `base`) — связь с пользователем и счётчики квот:
  - `profile` (relation → `users`) — владелец;
  - `token` (text) — случайное значение, сервер заполняет его при создании записи;
  - `total_requests`, `day_requests`, `day_reset_date`, `last_request_date` — учёт квот.
- **`entitlements`** (тип `base`) — права доступа, по одной записи на пользователя.

Обе записи сервер заводит сам: `tokens` — при первом запросе пользователя
(`/chat` или `/quota`), `entitlements` — при первой верификации покупки. Хук
PocketBase на `users.create` больше не нужен, но и не мешает: если запись уже
есть, сервер её просто использует.

На `profile` желательно повесить уникальный индекс: он страхует от гонки, если
сервер запущен в нескольких репликах и два первых запроса пользователя пришли
одновременно (внутри одного процесса создание уже сериализовано).

Коллекцию `chats` сервер не трогает — историю туда пишет клиент. Но рядом с
`events_json` и `entries_json` ей нужна текстовая колонка **`advice_json`**:
клиент уже отправляет её в `pushChat`, и без колонки `urgency`, `red_flags` и
`follow_up_questions` не переживут восстановление истории на новом
устройстве. Колонка добавляется руками в админке PocketBase.

### Коллекция `entitlements`

| Поле            | Тип     | Примечание                                            |
|-----------------|---------|-------------------------------------------------------|
| `user`          | text    | ID пользователя, **уникальный индекс**                 |
| `tier`          | text    | `free` / `premium`                                     |
| `active`        | bool    | Права действуют                                        |
| `product_id`    | text    | Код подписки в RuStore                                 |
| `purchase_id`   | text    | **Уникальный индекс** — по нему вебхук находит владельца |
| `order_id`      | text    |                                                        |
| `invoice_id`    | text    |                                                        |
| `state`         | text    | `ACTIVE` / `PAUSED` / `CLOSED` / `TERMINATED`          |
| `period`        | text    | `TRIAL` / `PROMO` / `MAIN` / `GRACE` / `HOLD`          |
| `expires_at`    | date    | Конец оплаченного периода                              |
| `auto_renewing` | bool    |                                                        |
| `source`        | text    | `rustore` / `manual`                                   |

Правила доступа коллекции нужно оставить пустыми (только суперпользователь):
сервер ходит в неё под админским токеном, а клиент получает права исключительно
через `GET /entitlement`. Иначе пользователь сможет выписать себе премиум сам.

Уникальный индекс по `user` обязателен — он страхует от гонки, если сервер
запущен в нескольких репликах. Премиум можно выдать вручную: создать запись с
`active=true`, `source=manual` и пустым `expires_at` (бессрочно) — такие права
сервер в RuStore не перепроверяет.

## Запуск

### Локально

```bash
cp .env.example .env   # заполнить значения
go run .
```

### Docker

```bash
docker build -t ai-server .
docker run --rm --env-file .env -p 8091:8091 -v $(pwd)/data:/app/data ai-server
```

### docker-compose

```bash
docker compose up -d
```

## CI/CD

При пуше в `main` пайплайн [`.github/workflows/deploy.yml`](.github/workflows/deploy.yml):

1. собирает Docker-образ (multi-stage, кеш слоёв в GitHub Actions);
2. публикует его в GitHub Container Registry (`ghcr.io`);
3. деплоит на сервер по SSH через `docker compose pull && up -d`.

Требуемые секреты репозитория: `SERVER_HOST`, `SERVER_USER`, `SERVER_SSH_KEY`.

В продакшене сервис работает за reverse-proxy [Caddy](https://caddyserver.com/), который терминирует TLS и проксирует на `127.0.0.1:8091`.

## Стек

Go 1.22 · [Gin](https://github.com/gin-gonic/gin) · [bbolt](https://github.com/etcd-io/bbolt) · [godotenv](https://github.com/joho/godotenv) · PocketBase · GigaChat API · Docker · GitHub Actions · Caddy
