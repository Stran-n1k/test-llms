# Независимое техническое ревью — сервис бронирования мест (Go + PostgreSQL)

Дата ревью: 07.11.2026. Ревью проводилось по `REVIEW_INSTRUCTIONS.md` и `TASK.md`;
исходники, тесты, зависимости, TASK.md и README проверяемого проекта не изменялись.
Собственные проверки выполнялись во временной папке `/tmp/opencode` и в отдельных
контейнерах/томах Docker; все созданные ресурсы удалены после проверки.

## 1. Краткое заключение

Решение **соответствует TASK.md**. Все обязательные свойства подтверждены как
автоматическими тестами проекта, так и независимыми проверками ревьюера на
настоящей PostgreSQL, включая два экземпляра сервиса с общей базой:

- сохранность данных после перезапуска — подтверждена (чистый рестарт и SIGKILL);
- отсутствие перепродажи при конкурентных бронированиях — подтверждено (60
  конкурентных запросов на 10 мест через два экземпляра: ровно 10 броней, 50
  отказов `not_enough_seats`);
- идемпотентность создания брони — подтверждена (30 одновременных одинаковых
  запросов: одна бронь, 29 replay с тем же id; повтор после отмены возвращает
  отменённую бронь без повторного списания);
- отмена ровно один раз при повторных и одновременных запросах — подтверждено
  (40 одновременных DELETE через два экземпляра, места возвращены один раз);
- два экземпляра с общей PostgreSQL — подтверждено (реальные контейнеры, общая БД);
- валидация и «понятные ошибки» — подтверждена (проверен весь заявленный набор
  кодов ошибок на живом сервере);
- подготовка схемы на пустой базе и повторный запуск без потери данных —
  подтверждена (включая одновременный старт двух экземпляров на чистой базе).

Подтверждённых дефектов уровней P0–P2 **нет**. Найдено четыре минимальных
дефекта уровня P3 (документационные расхождения и косметика), ни один не
нарушает обязательные свойства TASK.md.

## 2. Дефекты

### P3-1. README неверно описывает поведение 405

- Файл/строки: `README.md:378–379` («`405 Method Not Allowed` для известного
  пути с неверным методом формируется стандартным `net/http` как текст»).
- Условие проявления: любой запрос с методом, не совпадающим с
  зарегистрированными, на известный путь.
- Последствия: расхождение документации с фактическим поведением; клиент,
  рассчитывающий на 405/текст, получит иное.
- Доказательство (живой сервер, compose-стенд ревьюера):
  ```
  $ curl -s -o /dev/null -w '%{http_code} %{content_type}\n' -X PUT  http://localhost:18080/events
  404 application/json; charset=utf-8
  $ curl -s -o /dev/null -w '%{http_code} %{content_type}\n' -X PATCH http://localhost:18080/events
  404 application/json; charset=utf-8
  $ curl -s -o /dev/null -w '%{http_code} %{content_type}\n' -X DELETE http://localhost:18080/events
  404 application/json; charset=utf-8
  ```
  Причина: catch-all `mux.HandleFunc("/", s.handleNotFound)` (`internal/api/api.go:69`)
  перехватывает запросы, для которых путь совпадает, а метод — нет, и возвращает
  JSON `not_found`. 405 от `net/http` не возникает вообще.
- Направление исправления: поправить README (фактическое поведение — JSON 404 —
  само по себе консистентнее) либо убрать catch-all для известных путей и
  отдавать 405 с `Allow`.

### P3-2. Сообщения `invalid_json` раскрывают внутренние имена типов Go

- Файл/строки: `internal/api/errors.go:117`
  (`badRequest("invalid_json", "invalid JSON body: "+err.Error(), "")`).
- Условие проявления: любой запрос с JSON, не совпадающим по типу поля.
- Последствия: клиенту отдаются имена внутренних структур
  (`createBookingRequest.seats`) и сырой текст ошибки декодера. Это не утечка
  секретов, но раскрытие лишних внутренних деталей и нестабильный для клиента
  текст.
- Доказательство:
  ```
  $ curl -s -X POST .../events/{id}/bookings -H 'Content-Type: application/json' -H 'Idempotency-Key: t' -d '{"seats":1.5}'
  {"error":{"code":"invalid_json","message":"invalid JSON body: json: cannot unmarshal number 1.5 into Go struct field createBookingRequest.seats of type int"}}
  ```
- Направление исправления: вернуть нейтральное сообщение («request body does not
  match the expected format») либо маппить типовые ошибки декодера в коды по полю.

### P3-3. Откат транзакций выполняется с контекстом запроса

- Файл/строки: `internal/store/store.go:200` (defer rollback в CreateBooking),
  `internal/store/store.go:277` (`_ = tx.Rollback(ctx)` в ветке 23505),
  `internal/store/store.go:307` (defer rollback в CancelBooking).
- Условие проявления: истечение 15-секундного дедлайна запроса
  (`internal/api/api.go:20`, middleware `withTimeout`) или обрыв соединения
  клиентом до завершения транзакции.
- Последствия: `Rollback` с отменённым контекстом может не выполниться;
  соединение возвращается в пул в состоянии незавершенной транзакции и
  восстанавливается только средствами пула (health check / закрытие). Целостность
  данных не страдает — незакоммиченная транзакция в PostgreSQL отбрасывается в
  любом случае; риск только в кратковременной деградации пула.
- Доказательство: код-ревью; воспроизведение на живом сервере не проводилось
  (потребовало бы искусственной задержки в БД), поэтому оценка последствий — по
  семантике pgx.
- Направление исправления: `defer func() { _ = tx.Rollback(context.Background()) }()`.

### P3-4. Ограничение длины Idempotency-Key проверяется в байтах, а не символах

- Файл/строки: `internal/api/validation.go:116–118` (`len(key) > maxIdempotencyKeyLen`)
  против `README.md:249` («Клиент передаёт `Idempotency-Key` (1–200 символов)»).
- Условие проявления: ключ из многобайтовых символов UTF-8 суммарной длиной
  более 200 байт (например, 101 кириллический символ = 202 байта).
- Последствия: валидный по документации ключ отклоняется с
  `validation_error`. Для ASCII-ключей (типичный случай) поведение совпадает с
  документацией.
- Доказательство: `len()` в Go возвращает байты; проверка `utf8.ValidString` выше
  допускает многобайтовые ключи, т.е. противоречие реально. На живом сервере
  проверен граничный случай: 201 ASCII-символ → 400 `validation_error`.
- Направление исправления: `utf8.RuneCountInString(key) > maxIdempotencyKeyLen`
  (как сделано для `name`) либо скорректировать README на «байт».

## 3. Неподтверждённые риски

- **Неограниченное ожидание advisory lock при старте.**
  `internal/migrate/migrate.go:107–122`: `acquireLock` крутится до отмены
  контекста, а контекст `main` (`cmd/server/main.go:36`) не имеет дедлайна.
  Если другая сессия держит lock бессрочно (например, после неудачного
  `pg_advisory_unlock` в `migrate.go:48–51`), экземпляр зависнет на старте без
  таймаута и диагностики. Предпосылка редкая (нужен сбой unlock при живом
  соединении), воспроизвести не пытался — теоретический сценарий.
- **Миграции без `IF NOT EXISTS`.** `internal/migrate/migrations/0001_init.sql`
  использует `CREATE TABLE` без `IF NOT EXISTS`. Безопасность обеспечивается тем,
  что DDL и запись в `schema_migrations` коммитятся одной транзакцией, но при
  внешнем ручном изменении БД (таблицы есть, записи нет) старт завершится
  постоянной ошибкой. Требует out-of-band вмешательства, к работе приложения
  отношения не имеет.
- **Неограниченные списки и отсутствие rate limiting на публичной сети.**
  `internal/store/store.go:112–131, 147–177`: `GET /events` и
  `GET /events/{id}/bookings` без пагинации; лимита на частоту запросов нет.
  Анонимный клиент может создать большое число событий и вынудить большие
  ответы. В README явно задокументировано как допущение (запуск за обратным
  прокси, малые объёмы) — это условие развёртывания, а не дефект задания.

## 4. Условия развёртывания (не дефекты выполнения задания)

- Отсутствие аутентификации и TLS прямо разрешено TASK.md и задокументировано в
  README. Для публичной эксплуатации обязательны аутентификация, TLS и
  ограничение частоты запросов на обратном прокси.
- `compose.yaml:10–11` публикует PostgreSQL на всех интерфейсах с dev-паролем
  `booking:booking` — любой клиент сети получает полный доступ к данным.
  Задокументировано как dev-настройка; для эксплуатации публикацию порта БД
  нужно убрать.
- `customer_name`/`customer_email` заполняются произвольным клиентом —
  задокументировано.

## 5. Необязательные рекомендации

- В ветке гонки одинакового `Idempotency-Key` с другим телом клиент при
  нехватке мест может получить `not_enough_seats` вместо
  `idempotency_key_conflict` (проверка ключа идёт до списания, но конфликт
  выявляется только по уникальному индексу после UPDATE). Оба ответа — 409,
  задание не нарушается; при желании можно сделать код ошибки детерминированным.
- `api.Store` (`internal/api/api.go:24–33`) смешивает бизнес-операции и `Ping`;
  health можно вынести в отдельную маленькую зависимость.
- Добавить пагинацию списков при росте объёмов (сейчас честно задокументировано).
- Добавить healthcheck для сервиса `app` в compose (сейчас только у postgres).
- В тестах не покрыты: replay брони после отмены (задокументированное поведение,
  ревьюер подтвердил его вручную на живом сервере) и 413 на тело >1 MiB
  (ревьюер подтвердил вручную).

## 6. Архитектура

**Соразмерность.** Решение соответствует масштабу задачи: ~1400 строк кода без
тестов, стандартная библиотека для HTTP, один драйвер БД, никакой лишней
инфраструктуры (нет фреймворков, генерации, DI-конфигураций). Слоёв ровно
столько, сколько нужно: `api` (транспорт, валидация, ошибки) → `store`
(SQL, транзакции, инварианты) → `model` (общие типы); `db`, `migrate`, `config`
— инфраструктурные пакеты. Отдельного service-слоя нет, и это правильное
упрощение: бизнес-логика здесь целиком сводится к транзакционным операциям над
двумя таблицами, промежуточный слой стал бы чистой пересылкой данных.

**Разделение ответственности и направление зависимостей.** Зависимости направлены
внутрь без циклов: `cmd/server` композирует всё; `api` не знает о pgx и опирается
на собственный интерфейс `Store`, объявленный на стороне потребителя
(`internal/api/api.go:24–33`) — идиоматичный Go-подход, дающий тестируемость HTTP
слоя без БД. `store` не знает о HTTP. Единственная мелкая нестыковка — `Ping` в
бизнес-интерфейсе (см. рекомендации).

**Размещение инвариантов и границы транзакций — сильнейшая часть решения.**
Ёмкость обеспечивается одним атомарным `UPDATE ... WHERE reserved_seats + $2 <=
capacity` (`internal/store/store.go:217–223`) с row-lock PostgreSQL; CHECK-констрейнты
в схеме (`0001_init.sql:12,15`) — вторая линия обороны. Бронь + списание мест —
одна транзакция; отмена — одна транзакция с `SELECT ... FOR UPDATE`
(`store.go:309–310`). Гонка одинаковых ключей разрешается уникальным индексом с
корректной обработкой 23505 (rollback с освобождением мест и повторным чтением,
`store.go:275–287`). Рассуждения о конкурентности зафиксированы в комментариях —
это редкость и большой плюс. Многопоточность целиком делегирована БД, общих
изменяемых состояний в процессах нет, поэтому гарантии переносятся на N
экземпляров без изменений.

**Тестируемость.** Высокая: интеграционные тесты поднимают настоящий PostgreSQL
(Testcontainers), каждому тесту — своя схема (`internal/testutil/postgres.go`),
два независимых пула + два HTTP-сервера честно моделируют два экземпляра. Валидация
покрыта юнит-тестами таблицей кейсов.

**Понятность и стоимость изменений.** Код читается легко, имена точные,
комментарии объясняют «почему», а не «что». README детальный и честный (за
исключением P3-1). Добавление поля потребует правки в model/store/валидации —
умеренная и предсказуемая цена. Дублирование `getBookingByKey` (tx/pool,
`store.go:355–367`) — два коротких варианта одной функции, приемлемо.

Избыточной сложности не выявлено; упрощений, мешающих поддержке, тоже — отсутствие
пагинации и очистки ключей осознанны и задокументированы.

## 7. Безопасность

**Модель угроз и границы доверия.** Сервис позиционируется как внутренний
backend без аутентификации (прямо разрешено TASK.md): HTTP-клиенты считаются
недоверенными, PostgreSQL и сеть между сервисом и БД — доверенными, публичный
доступ предполагается только через обратный прокси (задокументировано). Ниже —
проверки в этой модели.

**SQL-инъекции.** Все запросы параметризованы (`$1..$n`), конкатенаций с
недоверенными данными нет; миграции — статический `go:embed`. Живая проверка:
полезная нагрузка `Robert'); DROP TABLE events;--` в `name`, `x' OR '1'='1` в
`Idempotency-Key`, `a; DELETE FROM events;--` в `customer_name` — всё сохранено
как литеральные данные, таблицы целы, счётчики консистентны. Инъекций не найдено.

**Обработка недоверенного JSON и ограничения входных данных.** Строгий декодер:
`DisallowUnknownFields`, ровно один JSON-объект, EOF-проверка; тело ограничено
1 MiB (`http.MaxBytesReader`); `Content-Type: application/json` обязателен.
Подтверждено на живом сервере: 413 (>1 MiB), 415 (нет/неверный Content-Type),
400 (битый JSON, неизвестное поле, два объекта, `null`, `seats:1.5`,
`capacity:"10"`, переполнение int, control-символы в ключе, ключ >200 байт).
Числовые поля не допускают переполнения `int` и ограничены сверху (capacity,
seats ≤ 1 000 000), что исключает и переполнение `integer` в PostgreSQL
(сумма ≤ 2 000 000 < 2^31). Пути — только UUID через `uuid.Parse`.

**Таймауты и исчерпание ресурсов.** `ReadHeaderTimeout` 5s, `ReadTimeout` 15s,
`WriteTimeout` 30s, `IdleTimeout` 60s, `MaxHeaderBytes` 1 MiB
(`cmd/server/main.go:51–59`); на запрос — 15-секундный дедлайн, распространяемый
на запросы к БД (`internal/api/middleware.go:12–18`); пул соединений ограничен
(MaxConns 10). Проверено: заголовок 2 MB → 431; slowloris-атаки обрезаются
таймаутами заголовков. Нагрузочного тестирования сверх ~60 конкурентных запросов
не проводилось (вне рамок ревью).

**Утечки внутренних ошибок и секретов.** Все 500 — обезличенный
`internal_error`, детали только в серверные логи (`internal/api/errors.go:78–85`);
panic восстанавливается, стек пишется только в лог, клиенту — обезличенный 500.
Логи (`slog` JSON) содержат method/path/status/длительность — без PII
(customer_name/email не логируются). Единственная мелочь — P3-2 выше. Секретов в
коде нет; dev-пароль в compose задокументирован.

**Логирование.** Структтурированный JSON в stdout, уровень INFO для доступа,
ERROR для внутренних сбоев — достаточно для эксплуатации без излишеств.

**Контейнеры и порты.** Образ — distroless static с пользователем nonroot, без
shell; сборка с `-trimpath -ldflags "-s -w"`. Compose публикует 8080 (приложение)
и 5432 (PostgreSQL с dev-паролем) — см. условия развёртывания.

**Проверка зависимостей на известные уязвимости.** Инструмент:
`govulncheck` (`go run golang.org/x/vuln/cmd/govulncheck@latest ./...`),
результат: 4 уязвимости («Your code is affected»):
- GO-2026-6865 (klauspost/compress v1.18.0... v1.18.6, panic на фреймах) —
  фиксировано в v1.20.2;
- GO-2026-6355, GO-2026-6354 (golang.org/x/crypto/ssh v0.54.0, DoS канала) —
  фиксировано в v0.56.0;
- GO-2026-6253 (moby/go-archive v0.2.0, запись вне каталога при распаковке tar) —
  фиксировано в v0.3.0.

Применимость: все четыре достижмы только через цепочку тестовых зависимостей
Testcontainers (`internal/testutil` → `postgres.Run`/`Terminate`). Производственный
бинарник их не содержит — проверено `go list -deps ./cmd/server`: в рантайме только
`github.com/google/uuid`, `github.com/jackc/pgx/v5` (+puddle, pgpassfile,
pgservicefile) и `golang.org/x/text` (транзитивно через pgx); ни один
уязвимый модуль в этот список не входит (vendored `golang.org/x/crypto/*` —
пакеты стандартной библиотеки Go, не внешний модуль). Для рантайма сервиса
уязвимостей не выявлено; тестовый контур стоит обновить при удобстве
(`go get -u` тестовых зависимостей не проводил — исходники менять нельзя).

Отсутствие других проблем не означает доказанную безопасность — проверка
ограничена перечисленным объёмом.

## 8. Выполненные проверки и результаты

| # | Проверка | Команда | Результат |
|---|----------|---------|-----------|
| 1 | Статический анализ | `go vet ./...` | чисто |
| 2 | Сборка | `go build ./...` | ok |
| 3 | Форматирование | `gofmt -l ./cmd ./internal` | пусто |
| 4 | Целостность зависимостей | `go mod verify` | all modules verified |
| 5 | Полный набор тестов проекта | `go test ./... -count=1` | все PASS (integration 39s) |
| 6 | Интеграционные подробно | `go test ./internal/integration/... -count=1 -v` | 9/9 PASS (жизненный цикл, валидация, идемпотентность, 3 конкурентных сценария, конкурентная отмена, рестарт, миграции, health) |
| 7 | Запуск по README | `APP_PORT=18080 POSTGRES_PORT=15432 docker compose up --build -d` | оба контейнера подняты, `/healthz` 200, `/readyz` 200 |
| 8 | Функциональные curl-проверки | create event / booking / replay / availability / cancel | 201/201/200+`Idempotent-Replayed: true`/200/200, счётчики корректны |
| 9 | Сохранность при рестарте | `docker compose down` → `up -d` | данные (событие, бронь, счётчики) на месте |
| 10 | Сохранность при SIGKILL | `docker kill booking-app-1` → `up -d` | данные на месте |
| 11 | Два экземпляра + конкурентность | собственная программа (текст ниже) | oversell: 10/50, samekey: 1/29, cancel: 40×200, инварианты выполнены |
| 12 | Конкурентный старт на чистой БД | отдельный compose-проект + 2 контейнера приложения | миграцию применил ровно один экземпляр (advisory lock работает) |
| 13 | Валидация/безопасность по HTTP | curl-набор (см. раздел 7) | 413/415/400/431/409 — все коды соответствуют README |
| 14 | SQL-инъекции | полезные нагрузки в name/email/key | сохраняются литерально, схема цела |
| 15 | Уязвимости зависимостей | `govulncheck ./...` + `go list -deps ./cmd/server` | 4 в тестовой цепочке, 0 в рантайме |

Временный тест (конкурентные сценарии, два экземпляра; `/tmp/opencode/conc/main.go`,
запускался как `./conc <eventID> oversell|samekey|cancel <bookingID>` против
`localhost:18080` и `localhost:18081`):

```go
// Independent review load test: concurrent bookings against two instances.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

type bookingReq struct {
	Seats         int    `json:"seats"`
	CustomerName  string `json:"customer_name"`
	CustomerEmail string `json:"customer_email"`
}

type bookingResp struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

type errResp struct {
	Error struct {
		Code    string         `json:"code"`
		Details map[string]any `json:"details"`
	} `json:"error"`
}

func post(client *http.Client, url string, key string, seats int) (int, bookingResp, string) {
	body, _ := json.Marshal(bookingReq{Seats: seats, CustomerName: "Load", CustomerEmail: "load@example.com"})
	req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	resp, err := client.Do(req)
	if err != nil {
		fmt.Println("request error:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var b bookingResp
	var e errResp
	if resp.StatusCode >= 300 {
		_ = json.Unmarshal(data, &e)
		return resp.StatusCode, b, e.Error.Code
	}
	_ = json.Unmarshal(data, &b)
	return resp.StatusCode, b, ""
}

func main() {
	client := &http.Client{Timeout: 20 * time.Second}
	eventID := os.Args[1]
	mode := os.Args[2] // "oversell" | "samekey" | "cancel"
	urls := []string{
		"http://localhost:18080/events/" + eventID + "/bookings",
		"http://localhost:18081/events/" + eventID + "/bookings",
	}

	switch mode {
	case "oversell":
		// 60 concurrent single-seat bookings for capacity 10 across 2 instances.
		const attempts = 60
		var created, conflict, other int32
		ids := make(chan string, attempts)
		var wg sync.WaitGroup
		for i := 0; i < attempts; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				st, b, code := post(client, urls[i%2], fmt.Sprintf("load-%d", i), 1)
				switch st {
				case 201:
					atomic.AddInt32(&created, 1)
					ids <- b.ID
				case 409:
					atomic.AddInt32(&conflict, 1)
					if code != "not_enough_seats" {
						fmt.Println("unexpected conflict code:", code)
					}
				default:
					atomic.AddInt32(&other, 1)
					fmt.Printf("unexpected status %d code=%s\n", st, code)
				}
			}(i)
		}
		wg.Wait()
		close(ids)
		uniq := map[string]bool{}
		for id := range ids {
			uniq[id] = true
		}
		fmt.Printf("created=%d conflicts=%d other=%d uniqueBookings=%d\n",
			created, conflict, other, len(uniq))
		if created != 10 || conflict != 50 || len(uniq) != 10 {
			fmt.Println("FAIL: oversell invariant violated")
			os.Exit(1)
		}
		fmt.Println("PASS: exactly 10 booked, 50 refused")

	case "samekey":
		// 30 concurrent requests with the same key and body.
		var created, replayed, other int32
		var firstID string
		var mu sync.Mutex
		var wg sync.WaitGroup
		for i := 0; i < 30; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				st, b, code := post(client, urls[i%2], "same-key-load", 2)
				switch st {
				case 201:
					atomic.AddInt32(&created, 1)
					mu.Lock()
					firstID = b.ID
					mu.Unlock()
				case 200:
					atomic.AddInt32(&replayed, 1)
					mu.Lock()
					if firstID != "" && firstID != b.ID {
						fmt.Println("FAIL: replay returned different id", b.ID)
					}
					mu.Unlock()
				default:
					atomic.AddInt32(&other, 1)
					fmt.Printf("unexpected status %d code=%s\n", st, code)
				}
			}(i)
		}
		wg.Wait()
		fmt.Printf("created=%d replayed=%d other=%d\n", created, replayed, other)
		if created != 1 || replayed != 29 {
			fmt.Println("FAIL: same-key invariant violated")
			os.Exit(1)
		}
		fmt.Println("PASS: one booking, 29 replays")

	case "cancel":
		// 40 concurrent DELETEs of the same booking across both instances.
		bookingID := os.Args[3]
		var ok200, other int32
		var wg sync.WaitGroup
		for i := 0; i < 40; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				url := fmt.Sprintf("http://localhost:%d/bookings/%s", 18080+i%2, bookingID)
				req, _ := http.NewRequest("DELETE", url, nil)
				resp, err := client.Do(req)
				if err != nil {
					fmt.Println("request error:", err)
					os.Exit(1)
				}
				defer resp.Body.Close()
				io.Copy(io.Discard, resp.Body)
				if resp.StatusCode == 200 {
					atomic.AddInt32(&ok200, 1)
				} else {
					atomic.AddInt32(&other, 1)
					fmt.Println("unexpected cancel status:", resp.StatusCode)
				}
			}(i)
		}
		wg.Wait()
		fmt.Printf("ok200=%d other=%d\n", ok200, other)
		if other != 0 {
			fmt.Println("FAIL: unexpected statuses on cancel")
			os.Exit(1)
		}
		fmt.Println("PASS: all concurrent cancels 200")
	}
}
```

Результаты запуска (события capacity 10/10, экземпляры :18080 и :18081):

```
== oversell: created=10 conflicts=50 other=0 uniqueBookings=10  → PASS (reserved_seats=10)
== samekey:  created=1  replayed=29 other=0                      → PASS (reserved_seats=2, одна бронь)
== cancel:   ok200=40   other=0                                  → PASS (reserved_seats=0 — места вернулись один раз)
```

### Не выполненные проверки и причины

- **Режим `TEST_DATABASE_URL` (внешняя БД)** — не проверялся: режим Testcontainers
  полностью покрыл работу с настоящей PostgreSQL; дублирования сценария не требуется.
- **Нагрузочное тестирование сверх ~60 конкурентных запросов** — не проводилось:
  инструкция ревью запрещает тяжёлые нагрузочные атаки.
- **Активные атаки на чужие цели, TLS, реальный обратный прокси** — вне рамок:
  собственный стенд в VM, TLS в приложении отсутствует by design (TASK.md).
- **Fuzzing JSON-декодера** — не проводился; покрыт табличными случаями проекта и
  ручными проверками (раздел 7).
- **P3-3 (rollback с отменённым ctx)** — воспроизведение не проводилось (нужна
  искусственная задержка в БД); оценка дана по коду и семантике pgx, поэтому
  дефект помечен как код-ревью находка с оговоркой.

## 9. Сильные стороны решения

- **Корректное и хорошо обоснованное ядро конкурентности**: одна атомарная
  команда UPDATE для ёмкости, CHECK-констрейнты как вторая линия, `FOR UPDATE`
  для отмены, уникальный индекс `(event_id, idempotency_key)` с грамотной
  обработкой 23505 (откат с освобождением мест и корректный replay) — все
  гарантии делегированы PostgreSQL и потому автоматически масштабируются на
  несколько экземпляров. Рассуждения зафиксированы в комментариях кода и README.
- **Честная работа с настоящей БД в тестах**: Testcontainers, изоляция по схемам
  на каждый тест, два независимых пула и два HTTP-сервера для моделирования двух
  экземпляров; конкурентные сценарии (100 запросов на 10 мест, 30 одинаковых
  ключей, 40 одновременных отмен) проверяют именно заявленные гарантии.
- **Операционная зрелость**: graceful shutdown, liveness/readiness, встраивание
  миграций в бинарник с advisory lock, ожидание БД при старте с ретраями,
  distroless nonroot-образ, таймауты на все фазы HTTP-соединения.
- **Прозрачный и честный README**: полный набор решений, гарантий и ограничений,
  включая неудобные (бессрочные ключи, отсутствие пагинации, dev-секреты) —
  за единственным исключением P3-1.
- **Аккуратная валидация и единый формат ошибок**: строгий JSON, лимиты на все
  поля, `Idempotency-Key` с контролем UTF-8 и управляющих символов,
  структурированные коды ошибок с деталями (`not_enough_seats` с available/requested).
- **Соразмерная архитектура без избыточности** — подробнее в разделе «Архитектура».

## 10. Итог

Обязательные свойства TASK.md выполнены и подтверждены независимыми проверками на
настоящей PostgreSQL, включая два экземпляра сервиса. Подтверждённые дефекты
ограничиваются четырьмя минимальными (P3): два расхождения README с фактическим
поведением, косметическое раскрытие имён типов в ошибках и техническая деталь
отката транзакций, не влияющая на целостность данных. Дефектов P0–P2 не найдено.
