# Независимое техническое ревью

Дата проверки: 2026-11-07. Проверялась версия `45b2ae9` (`Review baseline`). Исходники, тесты, зависимости, `TASK.md` и `README.md` не изменялись.

## Краткое заключение

Решение в целом реализует требования `TASK.md`: HTTP JSON API работает, данные сохраняются после перезапуска, схема создаётся на пустой PostgreSQL и повторно применяется без потери данных, атомарный учёт ёмкости и блокировки отмены работают через общую базу для двух экземпляров. Перепродажа, дубликаты бронирований и повторное освобождение мест в выполненных проверках не воспроизвелись.

Подтверждены два дефекта:

- **P1** — при одновременных одинаковых запросах на последние места один запрос может получить `409 not_enough_seats` вместо документированного replay существующей брони. Данные при этом не дублируются, но клиент получает ложный отказ при фактически созданной брони.
- **P3** — тело больше 1 MiB в части случаев возвращает `400 invalid_json` вместо документированного `413 payload_too_large`.

Эти дефекты не отменяют подтверждённые гарантии сохранности данных, ёмкости и отмены, но первый требует исправления механизма идемпотентности и дополнительного теста.

## Подтверждённые дефекты

### P1 — конкурентный повтор на последние места возвращает ошибку вместо существующей брони

**Файл и строки:** `internal/store/store.go:202-211, 217-245, 270-285`; документированное поведение — `README.md:166-170, 247-257`. Пробел в тестах — `internal/integration/api_test.go:462-516`.

**Условие проявления:**

1. Два экземпляра получают одновременные запросы с одним `Idempotency-Key` и одинаковым телом.
2. Оба выполняют начальный поиск ключа до commit другого запроса и не находят бронь.
3. Запрошены все оставшиеся места, например `capacity = 2`, `seats = 2`.
4. Один запрос резервирует места и создаёт бронь. Второй не проходит условный `UPDATE events` из-за уже нулевой доступности.

**Последствия:** второй запрос получает `409 not_enough_seats`, хотя его бронь фактически создана первым запросом. Он не получает `200 OK`, `Idempotent-Replayed: true` и идентификатор существующей брони, которые обещаны README для гонки одинаковых запросов. Повтор после завершения первого запроса уже вернёт replay, поэтому проблема не приводит к дубликату или перепродаже, но делает результат безопасного повтора зависимым от тайминга и доступности.

Причина видна в коде: повторный поиск по ключу выполняется только до резервирования (`store.go:202-211`) или после unique violation при `INSERT` (`store.go:270-285`). Ветка неудачного `UPDATE events` (`store.go:224-245`) сразу возвращает `NotEnoughSeatsError`, не проверяя, не появилась ли бронь с тем же ключом.

Тот же пропущенный путь затрагивает одновременное использование одного ключа с **разными** телами: вместо документированного `idempotency_key_conflict` возможен `not_enough_seats`. Это воспроизведено отдельным тестом, описанным ниже.

**Доказательство:** временный тест на двух HTTP-серверах с независимыми пулами и настоящей PostgreSQL удерживал строку события внешней транзакцией, чтобы оба запроса прошли начальный поиск ключа, затем отпускал блокировку. Результат:

```text
request 1: status=409 replayed=""
  {"error":{"code":"not_enough_seats",...,"details":{"available_seats":0,"requested_seats":2}}}
request 2: status=201 replayed=""
  {"id":"7fb6fa4b-514f-4d0d-9d7a-8e470f12c5be",...,"seats":2,"status":"confirmed",...}
```

Ожидались статусы `201` и `200`; фактически получены `201` и `409`. После сценария остаётся одна бронь и `reserved_seats = 2`, то есть перепродажи нет. Полный текст и команда воспроизведения приведены в приложении 1.

Существующий `TestConcurrentIdempotentCreate` использует `capacity = 10`, `seats = 3`: у проигравших транзакций остаётся возможность пройти `UPDATE` и дойти до обработчика unique violation. Поэтому тест не покрывает ветку, где конкурентный replay отсекается раньше по ёмкости.

**Направление исправления:** при `pgx.ErrNoRows` после условного резервирования повторно проверить `(event_id, idempotency_key)` в согласованном состоянии транзакции до формирования `NotEnoughSeatsError`. Для найденной записи при совпадающем hash вернуть replay, при другом hash — `ErrIdempotencyConflict`. Добавить конкурентные тесты с `seats == capacity` для одинаковых тел и с нехваткой мест для разных тел через два независимых пула.

### P3 — часть тел больше 1 MiB получает 400 вместо 413

**Файл и строки:** `internal/api/errors.go:101-124`; контракт — `README.md:73-75, 208-221`.

**Условие проявления:** первый JSON-документ полностью помещается в лимит, а за ним следует достаточно длинный хвост, например корректный объект события и 1 MiB пробелов.

**Последствия:** возвращается `400 invalid_json` с сообщением `request body must contain exactly one JSON object`, хотя README задаёт `413 payload_too_large` для любого тела больше 1 MiB. Данные не создаются; проблема ограничена кодом и диагностикой ошибки.

**Доказательство:** против запущенного в Docker экземпляра отправлен корректный JSON события, дополненный 1 MiB пробелов. Ответ:

```http
HTTP/1.1 400 Bad Request
Content-Type: application/json; charset=utf-8

{"error":{"code":"invalid_json","message":"request body must contain exactly one JSON object"}}
```

`http.MaxBytesError` распознаётся только при первом `Decode` (`errors.go:105-114`). Ошибка второго `Decode`, проверяющего конец входа, без разбора превращается в `invalid_json` (`errors.go:121-124`). Воспроизведение приведено в приложении 3.

**Направление исправления:** одинаково обрабатывать `http.MaxBytesError` для обоих вызовов `Decode`; добавить HTTP-тест именно с корректным первым объектом и хвостом, превышающим лимит, а также с обычным вторым JSON-значением.

## Архитектура

### Соразмерность и разделение ответственности

Структура соразмерна небольшой задаче; дополнительный сервисный слой здесь не нужен:

- `internal/api` отвечает за маршруты, декодирование, валидацию и отображение ошибок. Зависимость от хранилища задана узким интерфейсом `api.Store` (`internal/api/api.go:23-33`), поэтому HTTP-слой можно тестировать отдельно.
- `internal/store` владеет SQL, транзакциями и инвариантами бронирования/отмены (`internal/store/store.go:188-348`). Это правильное размещение: атомарность нельзя обеспечить только последовательностью вызовов из HTTP-обработчиков.
- `internal/migrate` отдельно отвечает за версии схемы и advisory lock (`internal/migrate/migrate.go:35-121`), а `cmd/server` только собирает зависимости и управляет жизненным циклом.
- Направление зависимостей понятно: `cmd/server -> api/store/migrate/db`, при этом `api` зависит от абстракции хранилища и моделей, а не от HTTP-деталей внутри `store`.

Типы `model` содержат JSON-теги и поэтому слегка связывают хранилище с представлением API. Для одного JSON API это разумный компромисс, уменьшающий число почти одинаковых DTO; при появлении нескольких несовместимых представлений потребуется выделить транспортные типы.

### Бизнес-инварианты и транзакции

Сильное решение — сочетание инвариантов в Go и ограничений PostgreSQL:

- атомарный `UPDATE ... WHERE reserved_seats + seats <= capacity` сериализует бронирования одной строки события;
- создание брони и списание мест находятся в одной транзакции;
- отмена блокирует бронь `FOR UPDATE`, меняет статус и возвращает места в одной транзакции;
- `CHECK` и unique constraint в `internal/migrate/migrations/0001_init.sql:8-37` ограничивают некорректные состояния даже вне HTTP-валидации.

Компромисс — денормализованный `events.reserved_seats`: база проверяет границы счётчика, но не его равенство сумме активных броней. Текущие пути записи сохраняют согласованность транзакциями. При добавлении изменения числа мест, восстановления отменённой брони или новых статусов каждый такой путь обязан синхронно менять счётчик и покрываться конкурентными тестами. Это не текущий дефект, а явная стоимость будущих изменений.

### Поддерживаемость и упрощения

Код короткий, SQL и границы транзакций читаются непосредственно, названия ошибок соответствуют API. Дублирование `getBookingByKey` для пула и транзакции (`store.go:355-367`) невелико и оправдано разными интерфейсами запросов.

`ListEvents` и `ListEventBookings` загружают все записи в память (`store.go:111-130, 146-176`). README прямо ограничивает решение небольшими объёмами, поэтому отсутствие пагинации не считается ошибкой задания. При росте данных это первое место, где изменение API и хранилища станет необходимым: сейчас одновременно растут время запроса, память процесса и размер ответа.

### Тестируемость и качество автоматических тестов

Автоматические тесты содержательные: настоящая PostgreSQL через Testcontainers, отдельные схемы, два HTTP-сервера с разными пулами, проверки как HTTP-ответов, так и счётчика непосредственно в БД. Конкурентные тесты проверяют итоговую сумму мест, а не только отсутствие паники. Повторный запуск конкурентного набора десять раз прошёл.

Основной пробел — отсутствие граничного пересечения идемпотентности и нехватки мест, приведшего к P1. Автотесты также моделируют экземпляры через `httptest`, а не запускают два бинарника; для проверяемых SQL-инвариантов это достаточно, а фактический запуск двух контейнеров дополнительно выполнен в этом ревью. Одновременный старт миграций и реальные перезапуски compose полезно перенести в постоянный набор проверок, если стоимость такого CI допустима.

## Безопасность

### Модель угроз и границы доверия

Рассматривалась модель небольшого внутреннего сервиса: клиенты HTTP и весь JSON/заголовки/идентификаторы не доверенные; PostgreSQL, `DATABASE_URL`, встроенные SQL-миграции и Docker-образы относятся к доверенной инфраструктуре. Для публичного размещения предполагается обратный прокси и сетевые ограничения. Отсутствие авторизации прямо разрешено `TASK.md` и не отмечается как дефект.

### Проверка ввода и запросов

- Все значения из HTTP передаются в SQL параметрами `$1`, `$2` и т.д.; динамического SQL с пользовательскими строками не найдено. SQL в миграциях встроен в бинарник. Строка `x'); DROP TABLE events; --` была сохранена как обычное имя события, таблица осталась доступна.
- JSON декодируется строго: проверяются media type, неизвестные поля и второе значение; есть ограничение 1 MiB, валидация UUID, ёмкости, числа мест, длин строк, email и ключа. Найденное исключение в коде ошибки для oversized body описано как P3, а не как обход лимита.
- Тело и заголовки ограничены, у сервера заданы `ReadHeaderTimeout`, `ReadTimeout`, `WriteTimeout`, `IdleTimeout` (`cmd/server/main.go:51-59`), у каждого запроса — deadline 15 секунд (`internal/api/api.go:16-21`), у readiness — 2 секунды. Пул ограничен десятью соединениями на экземпляр (`internal/db/db.go:21-25`). Это уменьшает риск исчерпания соединений и зависших запросов.
- Клиент получает общий `internal_error`, а подробности остаются в серверном логе (`internal/api/errors.go:78-85`). В request-логах есть метод, путь, статус и размер, но не тело, email, имя клиента или `Idempotency-Key`. Проверка с намеренно неверным DSN показала, что pgx заменяет пароль на `xxxxx` в журнале.

### Подтверждённые условия и риски развёртывания

1. **PostgreSQL и приложение публикуются на всех интерфейсах хоста.** `compose.yaml:10-11, 27-28` задаёт `${POSTGRES_PORT:-5432}:5432` и `${APP_PORT:-8080}:8080`; фактический `docker inspect` показал `HostIp: ""` и binding `0.0.0.0`. В compose используются известные логин/пароль `booking/booking`. Если хост доступен из недоверенной сети и нет внешнего firewall, к базе можно подключиться напрямую с этими учётными данными. README раскрывает это как dev-настройку, поэтому это не дефект локального задания. Для реального развёртывания нужны отдельный секрет, закрытый порт PostgreSQL или привязка к доверенному интерфейсу и сегментация сети.
2. **Исчерпание ресурсов через рост данных остаётся возможным.** При доступности API недоверенным клиентам можно создавать записи без rate limit, а непагинированные списки целиком материализуются в память. Порог отказа нагрузочной проверкой не определялся, поэтому эксплуатируемая DoS не объявляется подтверждённой. Для публичного контура нужны ограничения на прокси, квоты/пагинация и мониторинг объёма ответов.
3. **Аутентификация и TLS отсутствуют намеренно.** Для публичной эксплуатации их нужно обеспечить на доверенной границе; без TLS содержимое запросов и ответов, включая информационные поля клиентов и idempotency keys, не защищено от просмотра сетевым наблюдателем.

### Контейнеры и зависимости

Финальный образ приложения использует distroless и `USER nonroot:nonroot` (`Dockerfile:19-25`), секреты в образ не копируются. Это положительная минимизация поверхности атаки. Дополнительные hardening-настройки вроде read-only root filesystem возможны, но не требуются заданием.

Выполнен `govulncheck ./...` (через `go run golang.org/x/vuln/cmd/govulncheck@latest`, загружена версия `v1.9.0`). Инструмент завершился с кодом 3 и сообщил четыре symbol results:

| Идентификатор | Модуль / исправление | Применимость по результатам проверки |
|---|---|---|
| `GO-2026-6865` | `github.com/klauspost/compress v1.18.6` / `v1.20.2` | Модуль присутствует в графе тестовых зависимостей; `go list -deps ./cmd/server` не показал его в production-сервере. Пути эксплуатации через HTTP API не подтверждены. |
| `GO-2026-6355`, `GO-2026-6354` | `golang.org/x/crypto v0.54.0` / `v0.56.0` | Найденные вызовы относятся к `x/crypto/ssh` через Testcontainers. Production-граф не содержит уязвимый SSH-путь; применимость ограничена тестовой инфраструктурой. |
| `GO-2026-6253` | `github.com/moby/go-archive v0.2.0` / `v0.3.0` | Вызовы относятся к Testcontainers и обработке архивов при запуске тестов. Это риск тестового supply chain при обработке вредоносного архива, а не подтверждённый путь атаки на booking API. |

Сканирование всего модуля включает тестовый tooling, поэтому строку «your code is affected» нельзя напрямую переносить на production binary. Обновление этих зависимостей рекомендуется для тестовой среды. Эти результаты не означают ни доказанной эксплуатируемости через API, ни отсутствия других уязвимостей.

## Неподтверждённые риски и необязательные рекомендации

- Не проверялась устойчивость к потере соединения непосредственно во время `COMMIT`; транзакционная логика и повтор по ключу рассчитаны на такой сценарий, но fault injection остаётся полезной дополнительной проверкой.
- Не проводилась нагрузочная оценка числа одновременных HTTP-соединений и больших непагинированных ответов; по коду подтверждено только отсутствие пагинации, а не конкретный предел памяти или времени отказа.
- Для более строгой воспроизводимости образов можно фиксировать базовые образы по digest. Текущие теги `golang:1.27-alpine`, `postgres:16-alpine` и distroless успешно собрались и запустились, но теги могут указывать на новые слои со временем.
- В постоянные тесты можно добавить одновременный первый запуск двух процессов на пустой базе и описанный выше fault injection. Существующая advisory lock реализация в отдельной ручной проверке сработала.
- Рекомендуется обновить тестовые зависимости до версий, закрывающих найденные `govulncheck` advisories, и повторить сканирование; приоритет зависит от доверия к образам и архивам в CI.

## Фактически выполненные проверки

### Статический анализ, сборка и штатные тесты

| Команда | Результат |
|---|---|
| `go version` | `go1.27.3 linux/amd64`. |
| `docker version --format '{{.Server.Version}}'` | Docker `29.9.2`. |
| `docker compose version` | Compose `v5.7.0`. |
| `go build -mod=readonly ./...` | Успешно. |
| `go vet -mod=readonly ./...` | Успешно, без замечаний. |
| `gofmt -d cmd internal` | Пустой вывод. |
| `git diff --check` | Успешно, пустой вывод. |
| `go test -mod=readonly ./... -count=1 -timeout=10m` | Все пакеты прошли; integration — за `5.092s`. |
| `go test -mod=readonly -race ./... -count=1 -timeout=10m` | Все пакеты прошли, race detector ошибок не сообщил. |
| `go test -mod=readonly ./internal/integration -run 'Concurrent' -count=10 -timeout=10m` | Успешно за `8.160s`. |

Первый запуск `go test -mod=readonly ./... -count=1` был ограничен внешним таймаутом 120 секунд во время первоначальной загрузки зависимостей/запуска инфраструктуры и не завершился. После загрузки зависимостей полный и отдельный интеграционный запуски прошли; этот таймаут не классифицирован как ошибка приложения.

### Настоящая PostgreSQL и контейнеры

1. `docker build -t kimi-review-booking:45b2ae9 .` — успешная сборка Dockerfile.
2. `APP_PORT=18080 POSTGRES_PORT=15432 docker compose -p kimi_review_45b2ae9 up --build -d` — приложение и PostgreSQL запущены; `/readyz` вернул 200.
3. Второй независимый контейнер запущен командой `docker run` с тем же `DATABASE_URL`, общей сетью и портом `18081`; `/readyz` обоих экземпляров вернул 200.
4. Временный Python-драйвер `/tmp/opencode/kimi_review_45b2ae9/check.py` выполнил через публичный HTTP API:
   - 60 одновременных запросов по 2 места на событие ёмкостью 25 через два контейнера: ровно **12 × 201**, **48 × 409**, `reserved_seats = 24`, `available_seats = 1`;
   - 40 одновременных одинаковых запросов по 3 места с одним ключом: **1 × 201**, **39 × 200**, один уникальный booking ID, `reserved_seats = 3`;
   - 40 одновременных отмен через два контейнера: все **200**, после них `reserved_seats = 0`;
   - повтор исходного create после отмены: **200**, `Idempotent-Replayed: true`, тот же ID и статус `cancelled`, повторного списания нет;
   - SQL-инъекционная строка в имени осталась данными, последующие запросы к событиям работали.
5. `docker compose -p kimi_review_45b2ae9 down`, затем `up -d` без удаления тома и `docker restart` второго приложения: созданные ранее событие и бронь прочитались через разные экземпляры, `reserved_seats = 2`, `available_seats = 5`; `schema_migrations` содержит одну строку `0001_init.sql`.
6. На отдельной пустой базе одновременно запущены два контейнера приложения: оба перешли к HTTP listen, один применил миграцию, второй дождался lock; в `schema_migrations` осталась одна версия.
7. Созданные ревью контейнеры, сети, тома и образы после проверок удалены по явно заданным именам; чужие ресурсы не затрагивались.

### Временные воспроизводящие тесты

Тесты добавлялись только в копию `/tmp/opencode/kimi_review_45b2ae9/src`, не в проверяемый проект.

```bash
cd /tmp/opencode/kimi_review_45b2ae9/src
go test -mod=readonly ./internal/integration \
  -run '^TestReviewConcurrentIdenticalRequestsAtCapacity$' \
  -count=1 -v -timeout=2m
```

Ожидаемо завершился `FAIL`: одинаковые запросы получили `409` и `201` вместо `200` и `201`; создана одна бронь.

```bash
go test -mod=readonly ./internal/integration \
  -run '^TestReviewConcurrentDifferentPayloadSameKey$' \
  -count=1 -v -timeout=2m
```

Ожидаемо завершился `FAIL`: конфликтующий запрос получил `not_enough_seats` вместо `idempotency_key_conflict`.

### Уязвимости и дополнительные проверки

- `go run golang.org/x/vuln/cmd/govulncheck@latest ./...` — четыре symbol results, перечисленные выше; exit code 3.
- `go list -mod=readonly -deps ./cmd/server` — уязвимые пути `klauspost/compress`, `moby/go-archive`, `testcontainers` и `x/crypto/ssh` в production-графе не найдены; в выводе есть только несвязанные vendored компоненты стандартной библиотеки с похожим префиксом `vendor/golang.org/x/crypto`.
- Запуск сервера с `postgres://booking:supersecret@localhost:notaport/...` — ожидаемая ошибка конфигурации, пароль в логе заменён на `xxxxx`.
- HTTP oversized-сценарий из приложения 3 — воспроизведён P3.

## Не выполненные проверки и ограничения

- Не выполнялись тяжёлые нагрузочные атаки, fuzzing и проверки внешних целей; активные запросы шли только к созданным для ревью сервисам.
- Не проводился fault injection с `kill -9`, разрывом сети в момент `COMMIT`, потерей питания или повреждением диска. Проверен обычный перезапуск контейнеров и повторное применение миграций, а не все аварийные режимы PostgreSQL.
- Не сканировались уязвимости ОС и базовых Docker-образов отдельным container scanner; проверены только Go-зависимости через `govulncheck`.
- Не разворачивались публичный TLS-прокси, аутентификация и production network policy, поскольку они выходят за рамки `TASK.md`; соответствующие условия эксплуатации перечислены отдельно.
- Не исчерпывающе проверялись все возможные варианты JSON и email; покрыты штатными тестами основные ошибки, строгие поля, media type и целевой oversized-сценарий.

## Сильные стороны

- Корректные транзакционные границы и использование блокировок PostgreSQL вместо локального состояния.
- Ограничения схемы дублируют ключевые инварианты и уменьшают вероятность сохранения повреждённых данных.
- Чёткий и документированный API с единым форматом ошибок, строгой валидацией и ограничениями тела/времени.
- Понятные встроенные миграции с версионированием, транзакциями и advisory lock.
- Хорошие интеграционные тесты на настоящей PostgreSQL, включая конкурентность, два пула и проверки счётчиков в БД.
- Воспроизводимый Dockerfile/compose запуск, nonroot distroless runtime и сохранение данных в именованном томе.

## Приложение 1 — тест конкурентного replay на последних местах

Файл `/tmp/opencode/kimi_review_45b2ae9/src/internal/integration/review_replay_test.go`:

```go
package integration

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReviewConcurrentIdenticalRequestsAtCapacity(t *testing.T) {
	schema := testDatabase.NewSchema(t)
	a := newInstance(t, schema)
	b := newInstanceWithPool(t, schema.NewPool(t))
	event := a.createEvent("identical concurrent requests at capacity", 2)

	// Force both requests to complete the initial idempotency-key lookup before
	// either can reserve the last two seats.
	lockTx, err := a.pool.Begin(context.Background())
	require.NoError(t, err)
	_, err = lockTx.Exec(context.Background(),
		"SELECT id FROM events WHERE id = $1 FOR UPDATE", event.ID)
	require.NoError(t, err)

	type result struct {
		status int
		header http.Header
		body   []byte
		err    error
	}
	results := make([]result, 2)
	var wg sync.WaitGroup
	for i, target := range []*instance{a, b} {
		wg.Add(1)
		go func(i int, target *instance) {
			defer wg.Done()
			status, header, body, err := target.createBooking(event.ID, "same-key-at-capacity", 2)
			results[i] = result{status: status, header: header, body: body, err: err}
		}(i, target)
	}

	time.Sleep(500 * time.Millisecond)
	require.NoError(t, lockTx.Rollback(context.Background()))
	wg.Wait()

	for i, result := range results {
		require.NoError(t, result.err)
		t.Logf("request %d: status=%d replayed=%q body=%s",
			i+1, result.status, result.header.Get("Idempotent-Replayed"), result.body)
	}
	require.ElementsMatch(t, []int{http.StatusCreated, http.StatusOK},
		[]int{results[0].status, results[1].status})

	reserved, available := a.availability(event.ID)
	require.Equal(t, 2, reserved)
	require.Equal(t, 0, available)
	require.Len(t, a.listBookings(event.ID), 1)
}
```

## Приложение 2 — тест одного ключа с разными конкурентными телами

Файл `/tmp/opencode/kimi_review_45b2ae9/src/internal/integration/review_race_test.go`:

```go
package integration

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReviewConcurrentDifferentPayloadSameKey(t *testing.T) {
	schema := testDatabase.NewSchema(t)
	a := newInstance(t, schema)
	b := newInstanceWithPool(t, schema.NewPool(t))
	event := a.createEvent("same key, different concurrent payloads", 2)

	// Both HTTP requests can look up the unused key, but neither can reserve
	// seats until this transaction releases its lock on the event row.
	lockTx, err := a.pool.Begin(context.Background())
	require.NoError(t, err)
	_, err = lockTx.Exec(context.Background(),
		"SELECT id FROM events WHERE id = $1 FOR UPDATE", event.ID)
	require.NoError(t, err)

	type result struct {
		status int
		body   []byte
		err    error
	}
	results := make([]result, 2)
	var wg sync.WaitGroup
	send := func(i int, target *instance) {
		defer wg.Done()
		status, _, body, err := target.createBooking(
			event.ID, "same-key-different-payload", i+1)
		results[i] = result{status: status, body: body, err: err}
	}
	wg.Add(1)
	go send(0, a)
	time.Sleep(200 * time.Millisecond)
	wg.Add(1)
	go send(1, b)

	time.Sleep(500 * time.Millisecond)
	require.NoError(t, lockTx.Rollback(context.Background()))
	wg.Wait()

	for i, result := range results {
		require.NoError(t, result.err)
		t.Logf("request %d: status=%d body=%s", i+1, result.status, result.body)
	}
	require.ElementsMatch(t, []int{http.StatusCreated, http.StatusConflict},
		[]int{results[0].status, results[1].status})
	for _, result := range results {
		if result.status == http.StatusConflict {
			require.Equal(t, "idempotency_key_conflict", decodeError(t, result.body).Error.Code)
		}
	}

	reserved, available := a.availability(event.ID)
	require.Equal(t, 2, reserved+available)
	require.Len(t, a.listBookings(event.ID), 1)
}
```

## Приложение 3 — воспроизведение oversized-ответа

Против собственного запущенного экземпляра на `127.0.0.1:18080` использовался следующий фрагмент Python-драйвера:

```python
import json
import urllib.error
import urllib.request

body = json.dumps({
    "name": "size",
    "capacity": 1,
    "starts_at": "2026-12-01T18:00:00Z",
}).encode() + b" " * (1024 * 1024)
request = urllib.request.Request(
    "http://127.0.0.1:18080/events",
    data=body,
    method="POST",
    headers={"Content-Type": "application/json"},
)
try:
    with urllib.request.urlopen(request, timeout=30) as response:
        print(response.status, response.read().decode())
except urllib.error.HTTPError as error:
    print(error.code, error.read().decode())
```

Команда запуска: `python3 /tmp/opencode/kimi_review_45b2ae9/check.py` (тот же запрос входил в общий сценарий). Фактический результат: `400` и код `invalid_json`; ожидаемый по README результат: `413` и `payload_too_large`.
