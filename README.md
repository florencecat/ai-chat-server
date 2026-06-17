# ai-chat-server 

[![Build & Deploy](https://github.com/florencecat/ai-chat-server/actions/workflows/deploy.yml/badge.svg)](https://github.com/florencecat/ai-chat-server/actions/workflows/deploy.yml)

HTTP-сервис на Go, который проксирует запросы к [GigaChat API](https://developers.sber.ru/portal/products/gigachat-api) от Сбера. Принимает сообщения от клиента, проверяет аутентификацию и квоты пользователя через [PocketBase](https://pocketbase.io/), кеширует ответы и отдаёт результат языковой модели.

## Возможности

- **Проксирование к GigaChat** с автоматическим обновлением OAuth-токена (живёт 30 минут) и повтором запроса при `401`.
- **Аутентификация через PocketBase** — клиент присылает свой JWT, сервер верифицирует его и находит связанную запись пользователя.
- **Квоты** — лимиты «N запросов в минуту» и «N запросов в день» на пользователя, счётчики хранятся в PocketBase.
- **Кеширование** ответов в BoltDB с TTL — повторные одинаковые запросы не тратят квоту и не идут в GigaChat.
- **Санитизация ввода** — обрезка управляющих символов и ограничение длины сообщения.
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
│  2. Поиск записи tokens  ──────► PocketBase (по profile = user.id)
│  3. Проверка квоты                       │
│  4. Кеш (BoltDB) ─── hit ──► ответ       │
│  5. Запрос к модели      ──────► GigaChat API
│  6. Списание квоты       ──────► PocketBase
│                                          │
└─────────────────────────────────────────┘
```

Структура пакетов:

| Пакет        | Назначение                                          |
|--------------|-----------------------------------------------------|
| `config`     | Загрузка конфигурации из переменных окружения       |
| `gigachat`   | Клиент GigaChat: OAuth-токен, чат-запросы            |
| `pocketbase` | Клиент PocketBase: верификация пользователя, квоты   |
| `cache`      | Кеш ответов поверх BoltDB с TTL                      |
| `handlers`   | HTTP-обработчики (Gin)                               |

## API

Все запросы к `/chat` и `/quota` требуют заголовок `Authorization: Bearer <PocketBase JWT>`.

### `POST /chat`

```jsonc
// Request
{ "message": "Привет!" }
```

```jsonc
// 200 OK
{
  "response": { "...": "произвольный JSON от модели" },
  "cached": false,
  "quota": {
    "requests_today": 3,
    "limit_day": 15,
    "limit_minute": 1
  }
}
```

### `GET /quota`

Возвращает текущее состояние квоты пользователя (тело как в поле `quota` выше).

### `GET /health`

```json
{ "status": "ok" }
```

### Коды ошибок

| HTTP | `code`                | Когда                                       |
|------|-----------------------|---------------------------------------------|
| 400  | `INVALID_REQUEST`     | Невалидное тело запроса                      |
| 400  | `EMPTY_MESSAGE`       | Сообщение пустое после санитизации           |
| 401  | `MISSING_AUTH`        | Нет заголовка `Authorization`                |
| 401  | `UNAUTHORIZED`        | Невалидный или истёкший JWT                  |
| 403  | `TOKEN_NOT_FOUND`     | Для пользователя нет записи в `tokens`        |
| 429  | `RATE_LIMIT_MINUTE`   | Превышен лимит запросов в минуту             |
| 429  | `RATE_LIMIT_DAY`      | Превышена дневная квота                      |
| 503  | `UPSTREAM_RATE_LIMIT` | GigaChat вернул `429`                        |
| 500  | `GIGACHAT_ERROR`      | Ошибка обращения к GigaChat                  |

## Конфигурация

Все настройки задаются через переменные окружения (можно через `.env` — см. [`.env.example`](.env.example)).

| Переменная             | По умолчанию                  | Описание                                            |
|------------------------|-------------------------------|-----------------------------------------------------|
| `PORT`                 | `8080`                        | Порт HTTP-сервера                                   |
| `GIGACHAT_AUTH_KEY`    | —                             | Готовая Base64-строка «Авторизационные данные»      |
| `GIGACHAT_CLIENT_ID`   | —                             | Альтернатива `AUTH_KEY`: client id                  |
| `GIGACHAT_CLIENT_SECRET`| —                            | Альтернатива `AUTH_KEY`: client secret              |
| `GIGACHAT_SCOPE`       | `GIGACHAT_API_PERS`           | Scope доступа                                       |
| `GIGACHAT_MODEL`       | `GigaChat`                    | Модель                                              |
| `GIGACHAT_SKIP_TLS`    | `true`                        | Пропускать проверку TLS (самоподписанные сертификаты Сбера) |
| `SYSTEM_PROMPT`        | _(см. код)_                   | Системный промт                                     |
| `MAX_MESSAGE_LEN`      | `4000`                        | Максимальная длина сообщения                        |
| `CACHE_TTL`            | `1h`                          | Время жизни кеша                                    |
| `DB_PATH`              | `data/ai-server.db`           | Путь к файлу BoltDB                                 |
| `QUOTA_PER_MINUTE`     | `1`                           | Лимит запросов в минуту                             |
| `QUOTA_PER_DAY`        | `15`                          | Дневной лимит запросов                              |
| `PB_URL`               | `http://127.0.0.1:8090`       | URL PocketBase                                      |
| `PB_ADMIN_EMAIL`       | —                             | Email суперпользователя PocketBase                  |
| `PB_ADMIN_PASSWORD`    | —                             | Пароль суперпользователя PocketBase                 |

> Нужно задать **либо** `GIGACHAT_AUTH_KEY`, **либо** пару `GIGACHAT_CLIENT_ID` + `GIGACHAT_CLIENT_SECRET`.

## PocketBase

Сервис рассчитан на две коллекции:

- **`users`** (тип `auth`) — пользователи, аутентификация по email/паролю.
- **`tokens`** (тип `base`) — связь с пользователем и счётчики квот:
  - `profile` (relation → `users`) — владелец;
  - `total_requests`, `day_requests`, `day_reset_date`, `last_request_date` — учёт квот.

Запись в `tokens` нужно создавать при регистрации пользователя (например, через хук PocketBase на событие `users.create`).

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
