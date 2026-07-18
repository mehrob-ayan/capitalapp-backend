# Capital App — Backend

API для Telegram Mini App учёта личного капитала. Go + Echo + PostgreSQL (GORM).
Авторизация через проверку Telegram `initData` (HMAC), сессия — JWT. Все финансовые
расчёты (капитал, поток, доходность, амортизация/начисление процентов) — на сервере.

## Быстрый старт

```bash
docker compose up -d          # PostgreSQL
cp .env.example .env          # ALLOW_DEV_LOGIN=true — вход без Telegram
go mod tidy
go run ./cmd/api              # http://localhost:8080/__health
go test ./...
```

## Переменные окружения

| Переменная | Назначение |
|---|---|
| `PORT` | порт API (8080) |
| `DATABASE_URL` | строка подключения к Postgres |
| `TELEGRAM_BOT_TOKEN` | токен бота от @BotFather (прод) |
| `JWT_SECRET` | секрет для подписи сессий |
| `ALLOW_DEV_LOGIN` | `true` — вход без Telegram (только dev) |
| `CORS_ORIGINS` | домены фронта через запятую, или `*` |

## Структура

```
cmd/api/            точка входа
internal/
  api/              Echo: сервер, роуты, middleware, хендлеры (assets, rates, history, goals)
  calc/             финансовое ядро (чистые функции + тесты): конвертация, метрики,
                    амортизация, начисление процентов, схемы кредита
  config/           конфиг из окружения
  db/               подключение и миграции (GORM)
  model/            модели БД
  telegram/         проверка initData
  token/            выпуск/разбор JWT
```

## Ключевые эндпоинты (`/api/v1`)

- `POST /auth/telegram` — вход, выдаёт JWT
- `GET /me`, `PATCH /me` — профиль, базовая валюта
- `GET /overview` — сводка капитала
- `GET|POST|PATCH|DELETE /assets[/:id]` — активы и долги
- `GET|PATCH /rates` — ручные курсы валют
- `GET /history` — история капитала по снимкам
- `GET|POST|PATCH|DELETE /goals[/:id]` — цели
