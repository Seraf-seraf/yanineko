# Yani Neko

Компактный Telegram-бот на Go, который отвечает от лица персонажа из `SYSTEM_PROMPT`. Точка входа — Vercel Function `api/webhook.go`; локальное выполнение — `vercel dev`.

## Поведение

- В личном чате бот отвечает на текстовые сообщения и поддерживаемые медиа-вложения.
- В группе отвечает только на сообщение с прямым `@username` бота или на reply к сообщению бота.
- Распознаёт фотографии, изображения-файлы, GIF и стикеры; для GIF и TGS-анимаций использует до четырёх равномерно выбранных кадров. Если пользователь отвечает боту на чужое медиа, оно также учитывается.
- Перед обычным ответом получает текстовое описание медиа через настроенную модель. В архиве диалога сохраняется описание, а загруженные файлы и кадры не сохраняются.
- Ответ в группе отправляется reply к исходному сообщению и в тот же forum topic.
- `/about` возвращает фиксированное описание без вызова OpenAI.
- Полный архив реплик раздельно по чату и теме форума хранится в SQLite; он не обрезается по числу сообщений или возрасту.
- Рабочее состояние Responses API хранится отдельно от архива и сжимается через standalone compaction при превышении установленного бюджета контекста.
- Для фактов о мире бот может вызвать локальный `search_world_memory`; модель сама выбирает необходимость поиска, сетевого поиска во время диалога нет.
- Повтор Telegram `update_id` использует сохранённый статус; неопределённую доставку бот не отправляет повторно вслепую.
- Ответ модели сохраняется и отправляется обычным текстом, максимум 4096 Unicode code points за одно сообщение.

## Требования

- Go 1.27.3; версия закреплена в `go.mod`.
- Vercel CLI для локального `vercel dev`.
- Удалённая SQLite-совместимая база Turso для работы в Vercel. Файловое SQLite-хранилище в каталоге функции непостоянно и не подходит для диалоговой истории.
- Telegram bot token и отдельный OpenAI API key.
- Для извлечения кадров из GIF-видеоанимаций и WEBM-стикеров Vercel Go-функция встраивает закреплённый компактный LGPL-сборочный `ffmpeg` в исполняемый файл и при необходимости распаковывает его в `/tmp`. В остальных средах сначала проверяются бинарник из каталога сборки и `ffmpeg` из `PATH`, затем встроенная копия на Linux amd64; если декодер недоступен, бот использует миниатюру, когда Telegram её предоставил. Сборка и лицензия поставляемого бинарника описаны в [third_party/ffmpeg](third_party/ffmpeg/README.md).

Runtime использует прямой Turso HTTP-драйвер и переменные `TURSO_DATABASE_URL` / `TURSO_AUTH_TOKEN`. База Turso в этом репозитории или в Vercel аккаунте не создавалась. PostgreSQL адаптер сохранён в `internal/adapters/storage/postgres` как альтернативный; текущий Vercel composition root его не вызывает. `DATABASE_URL` не переключает runtime на PostgreSQL.

## Переменные окружения

Список находится в [.env.example](.env.example). Все значения, кроме необязательного `OPENAI_BASE_URL` и сохранённого для PostgreSQL адаптера `DATABASE_URL`, обязательны для runtime:

| Переменная | Назначение |
| --- | --- |
| `TELEGRAM_BOT_TOKEN` | Токен, выданный BotFather. |
| `TELEGRAM_WEBHOOK_SECRET` | Секрет для заголовка `X-Telegram-Bot-Api-Secret-Token`; допустимы `A-Z`, `a-z`, `0-9`, `_`, `-`, длина 1–256. |
| `OPENAI_API_KEY` | OpenAI API key, не токен подписки ChatGPT. |
| `OPENAI_MODEL` | Имя модели для Responses API; для распознавания медиа модель должна принимать изображения. По умолчанию используется `gpt-6-luna`. |
| `OPENAI_REASONING_EFFORT` | Усилие reasoning: `none`, `low`, `medium`, `high`, `xhigh` или `max`; по умолчанию `low`. |
| `SYSTEM_PROMPT` | Инструкции персонажа; многострочное значение поддерживается. |
| `OPENAI_BASE_URL` | Необязательно; только проверенный endpoint, совместимый с Responses API. |
| `TURSO_DATABASE_URL` | URL удалённой Turso базы, выданный интеграцией. |
| `TURSO_AUTH_TOKEN` | Токен доступа к этой базе. |
| `DATABASE_URL` | Необязательно; валидируется для сохранённого PostgreSQL адаптера, но текущим runtime не используется. |

Модель по умолчанию использует `low` reasoning. Перед каждым ответом адаптер считает токены точным endpoint Responses API и держит рабочий запрос в бюджете 240 000 токенов вместе с резервом на вывод. При превышении бюджета он вызывает standalone compaction и повторно проверяет результат. Генерация задаёт `store:false` и отключает автоматическое усечение контекста. Встроенная локальная база `internal/knowledge/canon.json` хранит факты с источниками и отдельно помечает канон аниме и сведения манги; для поиска во время разговора сетевые запросы не выполняются.

Секреты задаются в Vercel Project Settings → Environment Variables. Для локального `vercel dev` создайте игнорируемый `.env.local` по [.env.example](.env.example), заполните значения и привяжите каталог к проекту Vercel. Не добавляйте `.env.local` в Git.

## Локальная проверка

```sh
go test ./...
go vet ./...
go test -race ./...
go build ./...
make fmt
```

`make dev` запускает `vercel dev`. Для обработки реального обновления локально нужны те же env values, доступная Turso база и публичный HTTPS tunnel; сам запуск локального сервера webhook у Telegram не устанавливает.

Тест SQLite адаптера использует локальный SQLite-compatible driver и не требует Turso credentials. PostgreSQL real-driver тест запускается отдельно, только при `YANINEKO_TEST_DATABASE_URL`, указывающем на loopback PostgreSQL базу с именем, начинающимся на `yanineko_test`.

## Подготовка бота и webhook

1. Создайте бота через BotFather и сохраните токен в менеджере секретов.
2. В группах оставьте включённым Telegram Privacy Mode: бот получает адресованные ему сообщения и replies; отключение режима не требуется.
3. Подключите Turso к Vercel проекту и внесите остальные переменные окружения. После подключения проверьте, что Vercel выставил `TURSO_DATABASE_URL` и `TURSO_AUTH_TOKEN`.
4. Используйте URL Function `https://<deployment-domain>/api/webhook`.
5. После отдельного разрешения на публикацию установите webhook через Bot API. Команда ниже читает параметры из переменных окружения и передаёт конфигурацию `curl` через stdin, чтобы не записывать токен в shell history:

   Перед запуском задайте `TELEGRAM_WEBHOOK_URL` равным полному публичному HTTPS URL функции.

   ```sh
   curl --fail-with-body --silent --show-error --config - <<EOF
   url = "https://api.telegram.org/bot${TELEGRAM_BOT_TOKEN}/setWebhook"
   request = "POST"
   data-urlencode = "url=${TELEGRAM_WEBHOOK_URL}"
   data-urlencode = "secret_token=${TELEGRAM_WEBHOOK_SECRET}"
   data-urlencode = "allowed_updates=[\"message\"]"
   EOF
   ```

6. Проверьте результат `getWebhookInfo` тем же способом:

   ```sh
   curl --fail-with-body --silent --show-error --config - <<EOF
   url = "https://api.telegram.org/bot${TELEGRAM_BOT_TOKEN}/getWebhookInfo"
   request = "GET"
   EOF
   ```

Эти шаги документируют конфигурацию; в рамках текущей реализации webhook в Telegram не устанавливался и deployment приложения не выполнялся.

## Сборка и функция

Vercel обнаруживает Go Function в `api/`. `vercel.json` задаёт `maxDuration: 55`; обработчик ограничивает время одного webhook 48 секундами, OpenAI запрос — 30 секундами, Telegram запрос — 6 секундами. Ошибки конфигурации, хранилища и внешних API возвращают безопасный HTTP-ответ; payload, токены и system prompt не логируются.
