# Автозапуск на macOS (launchd)

Три сервиса поднимаются сами при входе в систему и переживают перезагрузку/сон:

- **com.capitalapp.backend** — Go API + фронт (порт 8080), перезапускается при падении
- **com.capitalapp.tunnel** — публичный HTTPS-туннель (ngrok или cloudflared)
- **com.capitalapp.backup** — ежедневный дамп базы (14:00; если ноут спал — при пробуждении)

PostgreSQL держит сам Docker: в `docker-compose.yml` стоит `restart: unless-stopped`,
поэтому контейнер поднимается, как только запускается Docker Desktop. Включи в
Docker Desktop «Start Docker Desktop when you sign in».

## Установка

```bash
BASE=~/Ayan/capital-app

# 1. Собрать фронт и бинарник бэкенда
(cd $BASE/web && npm run build)
(cd $BASE/server && go build -o bin/api ./cmd/api)

# 2. Прод-конфиг: $BASE/server/.env
#    ALLOW_DEV_LOGIN=false, TELEGRAM_BOT_TOKEN=..., JWT_SECRET=..., WEB_DIR=../web/dist

# 3. Прописать свой туннель в $BASE/server/scripts/tunnel.sh (ngrok или cloudflared)

# 4. Папки и права
mkdir -p $BASE/logs ~/Library/LaunchAgents
chmod +x $BASE/server/scripts/*.sh

# 5. Поставить сервисы
cp $BASE/server/deploy/launchd/*.plist ~/Library/LaunchAgents/
launchctl load -w ~/Library/LaunchAgents/com.capitalapp.backend.plist
launchctl load -w ~/Library/LaunchAgents/com.capitalapp.tunnel.plist
launchctl load -w ~/Library/LaunchAgents/com.capitalapp.backup.plist
```

Перед установкой останови ручной `go run` (порт 8080 должен быть свободен).

## Управление

```bash
# статус
launchctl list | grep capitalapp
# логи
tail -f ~/Ayan/capital-app/logs/backend.log
tail -f ~/Ayan/capital-app/logs/tunnel.log
# перезапустить бэкенд (например, после пересборки)
launchctl kickstart -k gui/$(id -u)/com.capitalapp.backend
# снять сервис
launchctl unload -w ~/Library/LaunchAgents/com.capitalapp.backend.plist
# разовый бэкап прямо сейчас
launchctl start com.capitalapp.backup
```

## После изменений в коде

```bash
(cd ~/Ayan/capital-app/web && npm run build)              # если менялся фронт
(cd ~/Ayan/capital-app/server && go build -o bin/api ./cmd/api)
launchctl kickstart -k gui/$(id -u)/com.capitalapp.backend
```

## Заметки

- Пути в plist'ах абсолютные (`/Users/mehrob.latipov/...`). Если поменяешь
  расположение проекта или имя пользователя — поправь plist'ы и `scripts/backup.sh`.
- Пока ноут спит/выключен, сервис недоступен всем. Твои данные не теряются:
  догоняющий воркер достроит историю при следующем старте.
- Бэкапы: `~/Ayan/capital-app/backups/` (хранятся последние 30).
