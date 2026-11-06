# Независимое техническое ревью: сервис бронирования мест

Проверена базовая ревизия `e3b435c`. Исходники, тесты, зависимости, `TASK.md` и `README.md` не менялись. Свои проверки лежали во временной папке, их тексты приведены в приложении.

## 1. Заключение

**Решение соответствует `TASK.md`.** Все обязательные свойства подтверждены на настоящей PostgreSQL 17, в том числе на двух реальных процессах сервиса:

| Требование | Результат | Главное доказательство |
| --- | --- | --- |
| Сохранность данных | Выполнено | Данные пережили `restart app`, `restart db`, `down` + `up`. После трёх `kill -9` PostgreSQL под нагрузкой из 1685 подтверждённых бронирований не потеряно ни одно |
| Нет перепродажи | Выполнено | 25 раундов с `kill -9` сервиса: `available + активные места = capacity`, отрицательных остатков нет |
| Идемпотентность создания | Выполнено | Повтор с тем же ключом не создаёт дубль, в том числе при обрыве ответа и перезапуске |
| Отмена возвращает места один раз | Выполнено | Двойная одновременная отмена через оба экземпляра: `cancelled_at` у всех ответов совпадает, места возвращены один раз |
| Два экземпляра с общей БД | Выполнено | Блокировки и уникальность находятся в PostgreSQL. Проверено на двух и четырёх процессах |
| Валидация | Выполнено | Некорректные запросы получают 4xx и не меняют данные. Есть мелкие расхождения с README (D2) |
| Подготовка схемы и повторный запуск | Выполнено | Гонка 4 процессов на пустой БД, 3 раза; повторный старт данные не меняет; неизвестная версия схемы останавливает запуск |
| Воспроизводимость и тесты | Выполнено | Запуск из README и `scripts/test-integration.sh` прошли с чистого состояния Docker |

**Дефектов P0, P1 и P2 не найдено.** Подтверждено пять дефектов P3. Они не затрагивают гарантии целостности: это расхождения с README, вводящие в заблуждение сообщения, неполная диагностика и мусорный файл после команды из README.

## 2. Подтверждённые дефекты

Все дефекты имеют приоритет P3.

### D1. P3: не все ответы API в JSON

- **Где:** `internal/api/api.go:47-57`, `README.md:74`.
- **Условие:** путь не в канонической форме, например `//healthz` или `/events/../events/{id}`. `ServeMux` возвращает непустой шаблон для перенаправления, поэтому JSON-ветка не срабатывает.
- **Последствия:** клиент получает `307` и `text/html`, хотя README обещает, что все ответы API — JSON. Данные не страдают. Клиент, разбирающий только JSON, получит ошибку разбора.
- **Доказательство:** `curl -i http://127.0.0.1:$PORT//healthz` вернул `307 Temporary Redirect`, `Content-Type: text/html`, тело `<a href="/healthz">Temporary Redirect</a>.` Для `POST //events` тоже приходит `307`. Сценарий: `probe.py`, блок «routing».
- **Направление:** вернуть JSON-ответ для неканонического пути или уточнить README. Ответы `net/http` до вызова обработчика (`400` на испорченную строку запроса, `431`) остаются текстовыми, и это обычное поведение; README стоит уточнить и здесь.

### D2. P3: разбор JSON мягче, чем заявлено

- **Где:** `internal/api/api.go:113`, `internal/api/api.go:128-133`, `README.md:58-60`.
- **Условие:** особенности `encoding/json`: имена полей без учёта регистра, повторяющиеся поля, одиночные суррогаты в `\u`-экранировании.
- **Последствия:**
  - `{"NAME":"upper","CAPACITY":3}` принимается, хотя README обещает отвергать неизвестные поля.
  - При `{"name":"dup","capacity":1,"capacity":7}` молча побеждает последнее значение: событие создаётся с вместимостью 7.
  - `{"name":"\ud800","capacity":1}` сохраняется как `U+FFFD`, хотя комментарий в `api.go:113` говорит об отказе от такой подмены.

  Инварианты мест не нарушаются, но опечатка или неоднозначный запрос не получают понятной ошибки.
- **Доказательство:** все три запроса вернули `201`. Сценарий: `probe.py`, блок «JSON edge cases».
- **Направление:** отвергать такие документы на уровне токенов или уточнить README и убрать обещание из комментария.

### D3. P3: совет повторить запрос с `Idempotency-Key` выдаётся не только для бронирований

- **Где:** `internal/api/api.go:98-102`, `README.md:221`.
- **Условие:** таймаут или внутренняя ошибка на любом маршруте.
- **Последствия:** `POST /events` не принимает ключ и не идемпотентен, но клиент получает совет повторить запрос. Если исход неизвестен, следование совету может создать второе событие. Для `GET` совет бессмыслен.
- **Доказательство:** при остановленной БД `POST /events` вернул `500` и `retry booking creation with the same Idempotency-Key`. При замороженной БД `GET /events/{id}` вернул `503` и `retry with the same Idempotency-Key`. Сценарий: `faults.sh`, блоки D и E.
- **Направление:** нейтральное сообщение по умолчанию, совет про ключ только для `POST /bookings`.

### D4. P3: таймауты не логируются, а уход клиента логируется как ошибка сервера

- **Где:** `internal/api/api.go:98-102`, `internal/api/api.go:264-266`.
- **Условие:** таймаут БД, недоступность в `/healthz` или разрыв соединения клиентом во время запроса.
- **Последствия:** при зависшей БД оператор не видит в логах ничего, журнала запросов тоже нет. Обратная ситуация: обычный обрыв клиента даёт уровень `ERROR` и ветку `500`, что создаёт шум и может ложно срабатывать в алертах.
- **Доказательство:** блок D в `faults.sh`: три отказа (`POST /bookings`, `GET /events/{id}`, `/healthz`), `new ERROR log lines for three failures: 0`. Блок B: `curl --max-time 1` дал запись `"level":"ERROR","error":"context canceled"`.
- **Направление:** логировать таймауты и отказ health как предупреждения, `context.Canceled` обрабатывать отдельно с низким уровнем.

### D5. P3: команда из README оставляет неигнорируемый бинарник

- **Где:** `README.md:182`, `.gitignore:1`.
- **Условие:** выполнить `go build ./cmd/server` в корне репозитория.
- **Последствия:** появляется неотслеживаемый файл `server`, который легко случайно закоммитить.
- **Доказательство:** во временной копии после этой команды `git status --short` показывает `?? server`.
- **Направление:** использовать в разделе проверок `go build -o bin/server ./cmd/server`, как уже сделано в `README.md:46`, либо добавить файл в `.gitignore`.

## 3. Неподтверждённые риски и необязательные рекомендации

### Неподтверждённые риски

1. **Ложная готовность PostgreSQL при инициализации** (`compose.yaml:11`, `compose.test.yaml:11`).
   - Механизм подтверждён: `pg_isready` без `-h` идёт через unix-сокет и видит временный сервер образа, пока TCP ещё закрыт. В замедленной инициализации получилось `healthcheck_cmd_rc=0`, `tcp_rc=2`.
   - Естественно сбой не воспроизведён: 16 запусков из 16 приняли TCP-соединение сразу после `up --wait`.
   - Последствия ограничены: приложение перезапустится по `restart: unless-stopped`, интеграционный скрипт в редком случае может упасть.
   - Направление: добавить `-h 127.0.0.1` в команду проверки.

2. **Один долго заблокированный ряд события может занять весь пул** (пул по умолчанию 4 соединения при 2 ядрах).
   - Предпосылка: блокировку держит кто-то вне сервиса, например ручная транзакция. Через API так долго ряд не удержать: при 300 одновременных бронированиях на одно событие максимум составил 15 мс.
   - Доказательство: блок C в `faults.sh`. Четыре запроса к заблокированному событию заняли пул, запрос к другому событию и `/healthz` прождали около 9 секунд. Они не упали, а целостность сохранилась.
   - Направление: `lock_timeout` короче 10 секунд или отдельные соединения для health.

3. **`tx.Rollback(context.Background())` не ограничен по времени** (`internal/store/store.go:63`, `:139`, `:183`).
   - Откат может зависнуть, если сеть до БД пропала ровно между бизнес-ошибкой (например, `ErrSoldOut`) и откатом. Окно очень мало, воспроизвести не удалось.
   - При `docker pause` запросы завершались вовремя (10 секунд), так как отмена контекста закрывает соединение.

4. **Устаревшие зависимости и тулчейн с известными записями** (`Dockerfile:1`, `go.mod:5`, `go.mod:11`, `go.mod:13`). Применимость к этому коду не подтвердилась, подробности в разделе 5.

5. **Глобальные ключи идемпотентности** (`internal/store/schema.sql:13`). Поведение задокументировано и само по себе дефектом не является.
   - Два независимых клиента с одинаковым предсказуемым ключом и телом получат одно бронирование: второй увидит `200` и ID чужой брони.
   - Сценарий следует из проверенной логики повтора (`integration_test.go:243-247`). Отдельно с двумя клиентами его не разыгрывал, для сервера они неотличимы.
   - README рекомендует случайный ключ, но его пример `order-2026-001` выглядит как предсказуемый.

### Необязательные рекомендации

Требованиями `TASK.md` это не является.

- **Дополнить тесты.** Выжили 6 из 22 мутантов: неизвестная версия схемы принимается, нет таймаута обработчика, таймаут отображается в 500, `/healthz` не ходит в БД, нет заголовка `Allow`, текст внутренней ошибки уходит клиенту. Особенно полезен тест на сокрытие внутренних ошибок: регрессия здесь сразу станет утечкой.
- **Покрыть точку входа.** `run` и `store.Open` не покрыты (0%), общее покрытие 73.9%. Запуск через Compose автотестами не проверяется.
- **Согласовать время остановки.** `main.go:59` ждёт до 20 секунд, а `docker stop` по умолчанию даёт 10. Обработчик ограничен 10 секундами, транзакции атомарны, поэтому данные не страдают; `stop_grace_period` сделает обещание README точным.
- **Добавить журнал запросов** с методом, маршрутом, статусом и длительностью.
- **Убрать двойное чтение** при повторе: `store.go:146` и `:152` читают одну и ту же строку, достаточно одного `scanBooking` и сравнения полей.

## 4. Архитектура

**Общая оценка: решение соразмерно задаче.** Около 540 строк рабочего кода: `cmd/server` (сборка и жизненный цикл), `internal/api` (HTTP и валидация), `internal/store` (SQL и транзакции). Избыточных слоёв и абстракций нет.

### Что сделано хорошо

- **Инварианты и границы транзакций находятся там, где нужно.** Условный `UPDATE` (`store.go:166`, `:184`) вместе с `CHECK` и `UNIQUE` (`schema.sql:5`, `:13`) опирается на гарантии PostgreSQL, а не на память процесса. Транзакции не выходят за методы `Store`, поэтому обработчик не может оставить их открытыми.
- **Направление зависимостей простое:** `main → api → store`, циклов нет.
- **Порядок работы снижает риск исчерпания ресурсов.** Тело читается и проверяется до обращения к БД, медленный клиент не держит соединение пула.
- **Тесты практичные.** Отдельная схема на тест и два независимых пула дают реальную конкурентность без сложной инфраструктуры. 16 из 22 мутантов убиты, включая все мутанты основных гарантий (учёт мест, повтор, отмена, блокировка миграции).

### Компромиссы и упрощения

| Пример | Последствие | Компромисс и оценка |
| --- | --- | --- |
| `store.Event` и `store.Booking` содержат JSON-теги (`store.go:28-43`) | Модель БД совпадает с контрактом API; изменение формата затронет `store` | Для такого размера оправданно: отдельные DTO дали бы только лишний код. Пересмотреть, когда контракт и схема разойдутся |
| `Store.Pool` экспортирован, `api` вызывает `Pool.Ping` (`store.go:26`, `api.go:264`) | HTTP-слой знает про pgx; замена драйвера затронет `api` | Метод `Store.Ping` в одну строку убрал бы связь. Стоимость мала, срочности нет |
| `api` зависит от конкретного `Store`; юнит-тесты передают `nil` (`api_test.go:12`) | Юнит-тесты возможны только для ветвей до обращения к БД; таймауты и 500 не проверяются | Интерфейс не обязателен: эти ветви можно проверить на настоящей БД с блокировкой ряда, как в `faults.sh` |
| `allowedMethods` повторяет таблицу маршрутов (`api.go:36-41`, `:61-76`) | Новый маршрут надо добавлять в двух местах, иначе вместо 405 будет 404. Тест не заметит | Цена за JSON-ответы 405 при стандартном `ServeMux`. Приемлемо при 6 маршрутах |
| Ограничения продублированы в коде и схеме (`api.go:22`, `:171`, `:175`, `:217`; `schema.sql:3`, `:4`, `:12`, `:14`) | Изменение лимита требует правки кода и миграции; в `api.go` число `1000000` записано дважды | Дублирование с БД полезно как защита. Именованная константа в `api.go` дешевле всего |
| Счётчик `available` денормализован, инвариант со `SUM` поддерживается только кодом (`README.md:167`) | Чтение дешёвое, но прямая запись в БД может нарушить инвариант. Записи на одно событие идут последовательно | Разумно и задокументировано; триггер был бы лишней сложностью |
| Миграция: один файл и `switch` по версии (`store.go:74-85`) | Вторая миграция потребует новой ветки | Для первой версии это верно: инструмент миграций был бы избыточен, а таблица версий и блокировка уже есть |
| «Нет строки» после `INSERT ... ON CONFLICT` означает конфликт, но приходит как `ErrNotFound` (`store.go:143`) | Читателю нужно знать эту особенность | Логика верна и закрыта тестами; поясняющий комментарий стоил бы одну строку |

## 5. Безопасность

### Модель угроз и границы доверия

Отсутствие авторизации разрешено `TASK.md` и дефектом не считается.

- **Недоверенное:** всё, что приходит по HTTP (тело, заголовки, путь), и сами клиенты.
- **Доверенное:** оператор, `DATABASE_URL`, образы, PostgreSQL и сеть Compose.
- **Граница развёртывания:** в Compose API опубликован только на loopback, PostgreSQL наружу не опубликована.

Проверки выполнялись только против собственных локальных сервисов, тяжёлые нагрузочные атаки не проводились.

### Подтверждённые уязвимости

**Эксплуатируемых уязвимостей не найдено.** Это не доказывает, что их нет: проверка ограничена сценариями ниже и временем ревью.

### Что проверено

| Область | Результат |
| --- | --- |
| SQL-инъекции | Значения передаются параметрами; конкатенируются только константы (`store.go:121-196`). Попытки через имя, ID, `event_id` и ключ дали сохранение строки как текста или `400`; таблицы целы |
| Недоверенный JSON | Отвергаются: неверные типы, два документа, BOM, невалидный UTF-8, вложенность 8000 уровней. Расхождения описаны в D2 |
| Размер запросов | Тело больше 16 KiB, в том числе chunked: `413`. Заголовок 64 KiB: `431`. Непустое тело отмены: `400`. Ключ больше 128 символов: `400` |
| Таймауты | Медленный заголовок закрыт через 5.0 с, зависшее тело через 15.0 с (клиент получает закрытие соединения без JSON). Зависшая БД: `503` через 10 с. Запуск ограничен 60 с по коду, с зависшей БД не проверялся |
| Утечки ошибок | Текст SQL и драйвера клиенту не попал ни при остановке, ни при зависании БД |
| Секреты в логах | Пароль не появился ни при ошибочном URL (pgx маскирует его как `xxxxxx`), ни при неверном пароле, ни при падении БД. В логах остаются пользователь, имя базы и адрес, что для внутреннего журнала приемлемо |
| Секреты в репозитории | Поиск типовых шаблонов ничего не нашёл. Пароль в Compose локальный и отмечен в README |
| Идентификаторы | 122 случайных бита из `crypto/rand`, списков нет, ID работает как секрет для доступа |
| Контейнеры | Приложение работает под UID 10001, образ без исходников, сборка многоэтапная |
| Порты | `127.0.0.1:${PORT}->8080`, у БД опубликованных портов нет. Тестовая БД: случайный порт на loopback |

### Зависимости

Инструмент: `govulncheck` v1.9.0, база `vuln.go.dev` от 2026-11-05. **Он нашёл записи в используемых версиях**, поэтому заявлять об отсутствии уязвимостей нельзя. Проверка применимости:

| Запись | Уровень находки | Применимость к этому коду |
| --- | --- | --- |
| GO-2026-5004, pgx 5.7.6, исправлено в 5.9.2 | Символ | Не применимо в текущей конфигурации: нужны режим simple protocol (не по умолчанию) и SQL с dollar-quoted литералом. Таких литералов в коде нет (`grep` пуст). Оператору не стоит включать этот режим до обновления |
| GO-2026-5970 и GO-2026-6629, x/text 0.24.0 | Символ | С HTTP-входа не достижимо: цепочка идёт только через SCRAM и пароль из `DATABASE_URL`. GO-2026-6629 касается профиля Nickname, а здесь `OpaqueString` |
| GO-2026-4771 и GO-2026-4772, pgx `pgproto3` | Пакет | Вызова уязвимых символов (`Backend.Receive`) сканер не нашёл |
| 20 записей, в основном x/crypto `ssh`, `ssh/agent`, `openpgp` | Модуль | Пакеты не используются приложением |
| HTTP/2 в Go 1.25.14: GO-2026-6603, -6611, -6612, -6617 | Символ, тулчейн образа | Не применимо: сервер без TLS, h2c отклонён (`curl --http2-prior-knowledge` завершился ошибкой, `--http2` остался на HTTP/1.1) |
| GO-2026-6613, CONNECT в Go 1.25.14 | Символ, тулчейн образа | Не применимо: на CONNECT сервис отвечает только `404` или `405`, проверено |
| GO-2026-6608, GO-2026-6607, GO-2026-6809 в Go 1.25.14 | Символ, тулчейн образа | Multipart не используется, ECH на сервере нет, в Compose `sslmode=disable`. При TLS к внешней БД GO-2026-6809 стоит оценить заново |

Сканирование собранного бинарника дало 38 записей, но это завышенная оценка: бинарник собран с `-s -w`, и сканер указывает символы `ssh`, которых приложение не использует.

**Рекомендация:** обновить pgx, x/text, x/crypto и базовый образ. Сканер указывает исправления stdlib только в Go 1.26.x, а тег `golang:1.25-alpine` даёт 1.25.14. Судя по политике поддержки двух последних версий Go при текущей 1.27, ветка 1.25 исправлений больше не получает; это вывод, а не проверенный факт.

### Необязательное усиление контейнеров

Подтверждённые факты, не ошибки для локального Compose:

- Роль `booking` в Compose является суперпользователем PostgreSQL (`rolsuper = t`), так как это роль из `POSTGRES_USER` (`compose.yaml:5`). Отдельная роль без лишних прав уменьшит ущерб при будущей ошибке.
- Для `app` не заданы `read_only`, `cap_drop`, `no-new-privileges`, лимиты памяти и процессов, нет `HEALTHCHECK`.
- Базовые образы закреплены по тегам, а не digest. Это отмечено в `README.md:231-232`.

### Условия публичного развёртывания

Это не дефекты выполнения задания. Для внешней публикации нужны:

- аутентификация и проверка владельца брони: сейчас любой, кто знает ID, может смотреть и отменять (`README.md:218`);
- TLS на входе и `sslmode=verify-full` к внешней БД;
- ограничение частоты запросов, числа соединений и объёма данных: создание событий и бронирований не ограничено, в коде нет лимита соединений;
- собственные секреты вместо пароля из Compose, раздельные роли для миграции и работы;
- резервные копии и журнал запросов;
- явный `HTTP_ADDR` при запуске вне Docker: значение по умолчанию `:8080` слушает все интерфейсы.

## 6. Выполненные проверки

Окружение: Go 1.27.3, Docker Compose 5.7.0, `postgres:17-alpine`, 2 ядра. `$S` — временная папка ревьюера.

### Статические проверки и штатные тесты

| Команда | Результат |
| --- | --- |
| `go mod verify` | `all modules verified` |
| `go mod tidy -diff` | Код возврата 0, расхождений нет |
| `gofmt -l .`, `go vet ./...` | Замечаний нет |
| `go build -o $S/bin/server ./cmd/server` | Успешно |
| `go test -count=1 ./...` без БД | Успешно, интеграционные тесты пропущены |
| `REQUIRE_INTEGRATION=1 go test -run TestIntegrationLifecycle ./internal/api` без URL | Ожидаемый отказ: `TEST_DATABASE_URL is required` |
| `./scripts/test-integration.sh -v` | Все тесты прошли, контейнер и сеть удалены |
| `./scripts/test-integration.sh -definitely-bad-flag` | Код 1 передан наружу, ресурсы удалены |
| `timeout -s INT 3 ./scripts/test-integration.sh` | Остатков нет (код 124 принадлежит `timeout`) |
| `go test -race -count=10 ./internal/api` | Успешно, 8.0 с |
| `go test -race -count=3 -cpu 1,2,4 ./internal/api` | Успешно |
| `go test -coverpkg=./... -coverprofile=... ./...` | 73.9% |
| Go 1.24.13 в контейнере: `go vet`, `go test` с настоящей БД, `go build` | Успешно, заявление README о Go 1.24+ подтверждено |
| `python3 mutants/run.py` | 16 мутантов убито, 6 выжило |

### Запуск и сценарии вне штатных тестов

| Сценарий | Результат |
| --- | --- |
| `PORT=18080 docker compose -p claude-review-app up --build -d` с пустого состояния, сразу `curl /healthz` | `200`, ошибок в логе нет |
| Примеры из README | Работают: `201`, повтор `200` с тем же `id`, места возвращаются |
| `restart app`, `restart db`, `down` + `up -d` | Данные и идемпотентность сохранились, версия схемы одна |
| `docker compose run --rm -p 127.0.0.1:18081:8080 app` | Второй экземпляр видит те же данные; отмена через него и через первый возвращает один `cancelled_at`, места вернулись один раз |
| `chaos.py`: 2 процесса, 400 клиентов, `kill -9`, повторы с тем же ключом, двойные отмены | 25 раундов из 25 `ok=True`. В первых пяти 31–74 транспортных сбоя на раунд, 0 потерь, 0 дублей, 0 неверных отказов |
| `dbcrash.py`: 3 раза `kill -9` PostgreSQL на томе Compose | Подтверждено 1685, строк 1685, `lost=0`, инвариант верен. За время отказов 984 ответа `500`, 3 повтора получили `200` после потери ответа |
| Гонка 4 процессов на пустой БД, 3 раза | Все `200`, одна версия схемы, ошибок в логах нет |
| Версия схемы 2 в БД | Код 1, `unsupported database schema version 2`, данные целы |
| Чужая таблица `bookings` в пустой БД | Код 1, откат: новых таблиц нет, чужие данные целы |
| Плохие `DATABASE_URL` и `HTTP_ADDR`, отсутствие URL, неверный пароль, закрытый порт | Код 1, понятная ошибка, пароль не показан |
| SIGTERM и SIGINT при запросе в работе | Запрос завершён с `201`, код выхода 0 |
| `faults.sh` | A: `503` через 10 с, затем `201` и `200`, одна строка. B: при обрыве клиента строк нет. C и D: см. раздел 3 и D4. После возврата БД работа восстановилась. E: `500` или `503`, детали драйвера клиенту не попали |
| 300 одновременных бронирований на одно событие с 50 местами | 50 раз `201`, 250 раз `409`, p99 14 мс, максимум 15 мс |
| `probe.py` | Результаты в D1, D2 и разделе 5 |
| `healthrace.sh` | Механизм подтверждён, естественных сбоев 0 из 16 |
| `govulncheck ./...` с Go 1.27.3 | Код 3: 3 записи на уровне символов, 2 на уровне пакета, 20 на уровне модуля |
| `GOTOOLCHAIN=go1.25.14 govulncheck ./...` | 11, 6 и 22 записи соответственно |
| `govulncheck -mode=binary` для бинарника из образа | 38 записей, оценка завышена |

**Сбои моего окружения, не приложения:**

- В первом прогоне сигналов `wait` дал код 127, так как процесс запускался в подоболочке. Проверка повторена корректно.
- В первом прогоне обрыва клиента блокировка уже снялась. Проверка повторена.
- В `healthrace.sh` не было `bc`; скрипт исправлен и запущен снова.
- `docker stop` моей контрольной БД на tmpfs стирал её данные и менял порт. Это свойство моего контейнера.

Созданные мной контейнеры, тома, сети и образы удалены. Остались кэш сборки Docker, кэш модулей Go и скачанный тулчейн Go 1.25.14: общий кэш я не очищал, чтобы не задеть чужие данные. Чужие остановленные контейнеры `bt-ci-*` не затрагивались.

### Не выполнено

- **Сканирование образа на уязвимости ОС:** сканера нет в окружении, поэтому о пакетах Alpine вывода нет.
- **`go test -race` на Go 1.24 и 1.25:** в образе без C-компилятора запускалось с `CGO_ENABLED=0`. На Go 1.25 `go test` не запускался, образ только собран и проверен сценариями.
- **Запуск на разных машинах и сетевые разделения:** все процессы работали на одной VM; сбои сети имитировались только `docker pause` и остановкой.
- **TLS до PostgreSQL и другие версии PostgreSQL:** не проверялись, в Compose используется `sslmode=disable` и версия 17.
- **Тяжёлая нагрузка, поток соединений, длительная работа:** исключены условиями ревью.
- **Запуск при зависшей БД (таймаут 60 с):** не проверялся, вывод только по коду.
- **Зависание отката** из риска 3: не воспроизведено.

## 7. Сильные стороны

- Гарантии построены на атомарных условных обновлениях и ограничениях PostgreSQL, поэтому работают для любого числа экземпляров и пережили `kill -9` сервиса и БД.
- Механизм идемпотентности описан честно, включая неочевидные случаи: неудача не занимает ключ, повтор после отмены возвращает отменённую бронь.
- Подготовка схемы атомарна и закрыта advisory lock; неизвестная версия и чужие таблицы не приводят к потере данных.
- Строгая валидация и единый формат ошибок, внутренние ошибки клиенту не раскрываются.
- Продуманы таймауты сервера, лимиты тела и заголовков, корректное завершение и восстановление после перезапуска БД.
- Интеграционные тесты сильные: настоящая БД, два пула, конкурентные сценарии. Прошли 19 повторных прогонов с детектором гонок (10 подряд и 9 с `-cpu 1,2,4`) и убили все мутанты основных гарантий.
- Compose безопасен по умолчанию для локального запуска: только loopback, БД не опубликована, приложение не под root. Тестовый скрипт изолирован и убирает за собой только своё.
- README точен и содержит настоящие ограничения. Кроме D1, D2 и D5, расхождений с поведением не нашлось.

## Приложение: тексты временных проверок и команды

Скрипты лежали вне проекта. Перед запуском нужно собрать сервер и выбрать временную папку; порты `18080-18104` должны быть свободны.

```sh
cd ~/review/project
export S=$(mktemp -d)
go build -o "$S/server" ./cmd/server
```

Для `chaos.py` и `run.py` нужна собственная PostgreSQL:

```sh
docker run -d --name claude-review-pg -e POSTGRES_USER=rev -e POSTGRES_PASSWORD=rev -e POSTGRES_DB=rev \
  -p 127.0.0.1::5432 --tmpfs /var/lib/postgresql/data postgres:17-alpine
export PGADDR=$(docker port claude-review-pg 5432 | head -1)
```

Для `probe.py` и `dbcrash.py` нужен запущенный сервис:

```sh
# probe.py: сервер на своей БД
DATABASE_URL="postgres://rev:rev@$PGADDR/rev?sslmode=disable" HTTP_ADDR=127.0.0.1:18097 "$S/server" &
# dbcrash.py: Compose-проект
PORT=18080 docker compose -p claude-review-app up --build -d
```

### run.py: Мутационная проверка копий проекта

Команда: `TEST_DATABASE_URL="postgres://rev:rev@$PGADDR/rev?sslmode=disable" python3 run.py`

````python
#!/usr/bin/env python3
"""Mutation check. Applies one change at a time to a throw-away COPY of the
project (never to the project itself) and reports whether the test-suite notices."""
import os, shutil, subprocess, sys, tempfile

SRC = "/home/petr/review/project"
ST, API, SQL, MAIN = "internal/store/store.go", "internal/api/api.go", "internal/store/schema.sql", "cmd/server/main.go"
M = [
 ("no capacity guard in UPDATE", ST, " AND available >= $2`", "`"),
 ("cancel without status guard", ST, " AND status='active' RETURNING", " RETURNING"),
 ("no idempotency payload compare", ST, "if previousEvent != eventID || previousQuantity != quantity {", "if false {"),
 ("sold-out commits the insert", ST, "return Booking{}, false, ErrSoldOut", "_ = tx.Commit(ctx); return Booking{}, false, ErrSoldOut"),
 ("no advisory lock", ST, "SELECT pg_advisory_xact_lock(683401927351)", "SELECT 1"),
 ("no ON CONFLICT", ST, "ON CONFLICT (idempotency_key) DO NOTHING ", ""),
 ("cancel does not return seats", ST, "available=available+$2", "available=available+0*$2"),
 ("startup wipes bookings", ST, "\tcase 1:\n", "\tcase 1:\n\t\tif _, err = tx.Exec(ctx, `TRUNCATE bookings`); err != nil {\n\t\t\treturn err\n\t\t}\n"),
 ("unknown schema version accepted", ST, 'return fmt.Errorf("unsupported database schema version %d", version)', '_ = fmt.Sprint(); return tx.Commit(ctx)'),
 ("lost update (read then absolute write)", ST,
  "result, err := tx.Exec(ctx, `UPDATE events SET available=available-$2 WHERE id=$1 AND available >= $2`, eventID, quantity)",
  "var cur int\n\tif err = tx.QueryRow(ctx, `SELECT available FROM events WHERE id=$1`, eventID).Scan(&cur); err != nil {\n\t\treturn Booking{}, false, err\n\t}\n\tif cur < quantity {\n\t\treturn Booking{}, false, ErrSoldOut\n\t}\n\tresult, err := tx.Exec(ctx, `UPDATE events SET available=$2 WHERE id=$1`, eventID, cur-quantity)"),
 ("cancel timestamp overwritten on repeat", ST, "b, err = scanBooking(tx.QueryRow(ctx, `SELECT `+bookingColumns+` FROM bookings WHERE id=$1`, id))\n\t\tif err != nil {\n\t\t\treturn Booking{}, err\n\t\t}\n\t\treturn b, tx.Commit(ctx)",
  "b, err = scanBooking(tx.QueryRow(ctx, `UPDATE bookings SET cancelled_at=clock_timestamp() WHERE id=$1 RETURNING `+bookingColumns, id))\n\t\tif err != nil {\n\t\t\treturn Booking{}, err\n\t\t}\n\t\treturn b, tx.Commit(ctx)"),
 ("no handler timeout", API, "context.WithTimeout(r.Context(), 10*time.Second)", "context.WithTimeout(r.Context(), 10*time.Hour)"),
 ("timeout mapped to 500", API, "case errors.Is(err, context.DeadlineExceeded):", "case false:"),
 ("healthz never checks DB", API, "if err := a.store.Pool.Ping(r.Context()); err != nil {", "if false {\n\t\tvar err error\n\t\t_ = err"),
 ("no body limit", API, "r.Body = http.MaxBytesReader(w, r.Body, maxBody)", "_ = http.MaxBytesReader"),
 ("unknown fields accepted", API, "d.DisallowUnknownFields()", ""),
 ("replay answers 201", API, "status := 200", "status := 201"),
 ("no Allow header on 405", API, 'w.Header().Set("Allow", allowed)', '_ = allowed'),
 ("internal error text leaked", API, 'writeError(w, 500, "internal_error", "internal server error; retry booking creation with the same Idempotency-Key")', 'writeError(w, 500, "internal_error", err.Error())'),
 ("schema: no available>=0 CHECK", SQL, "CHECK (available >= 0 AND available <= capacity)", ""),
 ("schema: no key UNIQUE", SQL, "idempotency_key text NOT NULL UNIQUE", "idempotency_key text NOT NULL"),
 ("schema: no status/cancelled_at CHECK", SQL, "CHECK ((status = 'cancelled') = (cancelled_at IS NOT NULL))", "CHECK (true)"),
]

def main():
    env = dict(os.environ)
    env["REQUIRE_INTEGRATION"] = "1"
    survived = 0
    for name, path, old, new in M:
        d = tempfile.mkdtemp(prefix="mut-", dir=os.path.dirname(os.path.abspath(__file__)))
        try:
            w = os.path.join(d, "p")
            shutil.copytree(SRC, w, ignore=shutil.ignore_patterns(".git"))
            f = os.path.join(w, path)
            s = open(f).read()
            if s.count(old) < 1:
                print(f"NOT-APPLIED  {name}")
                continue
            s = s.replace(old, new, 1)
            if path == API and "10*time.Hour" not in s:
                pass
            open(f, "w").write(s)
            r = subprocess.run(["go", "test", "-count=1", "-timeout=120s", "./..."], cwd=w, env=env,
                               capture_output=True, text=True)
            out = r.stdout + r.stderr
            if "build failed" in out or "[setup failed]" in out:
                print(f"BUILD-FAIL   {name}: {out[:300]}")
            elif r.returncode == 0:
                survived += 1
                print(f"SURVIVED     {name}")
            else:
                fails = sorted({l.split()[2] for l in out.splitlines() if l.startswith("--- FAIL")})
                print(f"killed       {name}  <- {','.join(fails)}")
        finally:
            shutil.rmtree(d, ignore_errors=True)
    print("survived:", survived, "of", len(M))

main()
````

### chaos.py: Два процесса сервиса и `kill -9`

Команда: `python3 chaos.py "$S/server" claude-review-pg "$PGADDR" rev 25`

````python
#!/usr/bin/env python3
"""Black-box check: two real server processes, one PostgreSQL, kill -9 under load.
Usage: chaos.py <server-binary> <pg-container> <pg-addr> <pg-user> <rounds>
"""
import http.client, json, os, random, signal, subprocess, sys, threading, time, uuid

BIN, CONT, ADDR, USER, ROUNDS = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4], int(sys.argv[5])
PORTS = [18091, 18092]


def psql(db, sql):
    r = subprocess.run(["docker", "exec", CONT, "psql", "-U", USER, "-d", db, "-Atc", sql],
                       capture_output=True, text=True)
    if r.returncode:
        raise RuntimeError(r.stderr)
    return r.stdout.strip()


def start(db, port):
    env = dict(os.environ, DATABASE_URL=f"postgres://{USER}:{USER}@{ADDR}/{db}?sslmode=disable",
               HTTP_ADDR=f"127.0.0.1:{port}")
    return subprocess.Popen([BIN], env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


def call(port, method, path, body=None, key=None, timeout=25):
    c = http.client.HTTPConnection("127.0.0.1", port, timeout=timeout)
    try:
        h = {}
        data = None
        if body is not None:
            data, h["Content-Type"] = json.dumps(body), "application/json"
        if key:
            h["Idempotency-Key"] = key
        c.request(method, path, data, h)
        r = c.getresponse()
        return r.status, json.loads(r.read() or b"null")
    finally:
        c.close()


def wait_up(port):
    for _ in range(200):
        try:
            if call(port, "GET", "/healthz", timeout=1)[0] == 200:
                return
        except OSError:
            time.sleep(0.05)
    raise RuntimeError(f"{port} did not start")


def round_(n):
    db = f"chaos_{n}_{uuid.uuid4().hex[:8]}"
    psql(USER, f"CREATE DATABASE {db}")
    # Real processes race the migration on an empty database.
    procs = {p: start(db, p) for p in PORTS}
    for p in PORTS:
        wait_up(p)
    capacity = 150
    eid = call(PORTS[0], "POST", "/events", {"name": "chaos", "capacity": capacity})[1]["id"]
    outcome, ids, lock = {}, {}, threading.Lock()
    stats = {"transport_errors": 0, "5xx": 0, "dup_replies": 0}
    stop_kill = threading.Event()

    def book(i):
        rnd = random.Random(i)
        key, qty = f"chaos-{i}", rnd.randint(1, 3)
        for _ in range(100):  # retry with the SAME key until a definite answer
            port = rnd.choice(PORTS)
            try:
                st, body = call(port, "POST", "/bookings", {"event_id": eid, "quantity": qty}, key)
            except (OSError, http.client.HTTPException):
                with lock:
                    stats["transport_errors"] += 1
                time.sleep(0.05)
                continue
            if st in (200, 201):
                with lock:
                    outcome[key], ids[key] = "ok", body["id"]
                if rnd.random() < 0.4:  # concurrent double cancel via both instances
                    cancel_twice(body["id"], key)
                return
            if st == 409 and body["error"]["code"] == "insufficient_capacity":
                with lock:
                    outcome[key] = "soldout"
                return
            with lock:
                stats["5xx"] += 1
            time.sleep(0.05)
        with lock:
            outcome[key] = "undecided"

    def cancel_twice(bid, key):
        res = []

        def one(port):
            for _ in range(100):
                try:
                    st, b = call(port, "POST", f"/bookings/{bid}/cancel")
                    if st == 200:
                        res.append(b["cancelled_at"])
                        return
                except (OSError, http.client.HTTPException):
                    pass
                time.sleep(0.05)
                port = PORTS[0] if port == PORTS[1] else PORTS[1]
        ts = [threading.Thread(target=one, args=(p,)) for p in PORTS]
        [t.start() for t in ts]
        [t.join() for t in ts]
        with lock:
            if len(res) != 2 or res[0] != res[1]:
                stats["dup_replies"] += 1
            outcome[key] = "cancelled"

    def killer():
        rnd = random.Random(n)
        while not stop_kill.wait(rnd.uniform(0.05, 0.25)):
            p = rnd.choice(PORTS)
            procs[p].send_signal(signal.SIGKILL)
            procs[p].wait()
            time.sleep(0.05)
            procs[p] = start(db, p)
            wait_up(p)

    k = threading.Thread(target=killer)
    k.start()
    N = 400
    sem = threading.Semaphore(40)

    def guarded(i):
        with sem:
            book(i)
    ts = [threading.Thread(target=guarded, args=(i,)) for i in range(N)]
    [t.start() for t in ts]
    [t.join() for t in ts]
    stop_kill.set()
    k.join()
    time.sleep(0.2)
    # Reconcile with the database.
    row = psql(db, f"""SELECT e.available, e.capacity,
        COALESCE(SUM(b.quantity) FILTER (WHERE b.status='active'),0),
        COUNT(b.id), COUNT(DISTINCT b.idempotency_key),
        COUNT(b.id) FILTER (WHERE b.status='cancelled')
        FROM events e LEFT JOIN bookings b ON b.event_id=e.id WHERE e.id='{eid}' GROUP BY e.id""")
    avail, cap, active, rows, keys, cancelled = map(int, row.split("|"))
    sold = {k for k, v in outcome.items() if v == "soldout"}
    in_db = set(filter(None, psql(db, "SELECT idempotency_key FROM bookings").split("\n")))
    wrong_soldout = sold & in_db
    lost = {k for k, v in outcome.items() if v in ("ok", "cancelled")} - in_db
    undecided = [k for k, v in outcome.items() if v == "undecided"]
    expected_cancelled = sum(1 for v in outcome.values() if v == "cancelled")
    ok = (avail + active == cap and avail >= 0 and rows == keys and not wrong_soldout
          and not lost and not undecided and cancelled == expected_cancelled
          and stats["dup_replies"] == 0)
    api_avail = call(PORTS[1], "GET", f"/events/{eid}")[1]["available"]
    ok = ok and api_avail == avail
    print(f"round {n}: ok={ok} available={avail} capacity={cap} active_seats={active} rows={rows} keys={keys} "
          f"cancelled={cancelled}/{expected_cancelled} soldout={len(sold)} wrong_soldout={len(wrong_soldout)} "
          f"lost={len(lost)} undecided={len(undecided)} {stats}", flush=True)
    for p in procs.values():
        p.terminate()
        p.wait(timeout=25)
    psql(USER, f"DROP DATABASE {db} WITH (FORCE)")
    return ok


sys.exit(0 if all([round_(i) for i in range(ROUNDS)]) else 1)
````

### dbcrash.py: `kill -9` PostgreSQL на томе Compose

Команда: `python3 dbcrash.py 18080 claude-review-app-db-1 booking`

````python
#!/usr/bin/env python3
"""kill -9 PostgreSQL (Compose volume) under load: every acknowledged booking must survive.
Usage: dbcrash.py <port> <db-container> <compose-db-user>"""
import http.client, json, subprocess, sys, threading, time, uuid

PORT, DB, USER = int(sys.argv[1]), sys.argv[2], sys.argv[3]


def call(method, path, body=None, key=None):
    c = http.client.HTTPConnection("127.0.0.1", PORT, timeout=15)
    try:
        h = {}
        data = None
        if body is not None:
            data, h["Content-Type"] = json.dumps(body), "application/json"
        if key:
            h["Idempotency-Key"] = key
        c.request(method, path, data, h)
        r = c.getresponse()
        return r.status, json.loads(r.read() or b"null")
    finally:
        c.close()


def psql(sql):
    return subprocess.run(["docker", "exec", DB, "psql", "-U", USER, "-d", USER, "-Atc", sql],
                          capture_output=True, text=True, check=True).stdout.strip()


cap = 100000
eid = call("POST", "/events", {"name": "crash", "capacity": cap})[1]["id"]
acked, statuses, lock = {}, {}, threading.Lock()
stop = threading.Event()
prefix = uuid.uuid4().hex[:8]


def worker(w):
    i = 0
    while not stop.is_set():
        key = f"{prefix}-{w}-{i}"
        i += 1
        while True:  # same key until a definite answer
            try:
                st, b = call("POST", "/bookings", {"event_id": eid, "quantity": 1}, key)
            except (OSError, http.client.HTTPException):
                st, b = "transport", None
            with lock:
                statuses[st] = statuses.get(st, 0) + 1
            if st in (200, 201):
                with lock:
                    acked[key] = b["id"]
                break
            time.sleep(0.1)


ts = [threading.Thread(target=worker, args=(w,)) for w in range(8)]
[t.start() for t in ts]
for crash in range(3):
    time.sleep(1.5)
    subprocess.run(["docker", "kill", "-s", "KILL", DB], check=True, capture_output=True)
    time.sleep(1)
    subprocess.run(["docker", "start", DB], check=True, capture_output=True)
    for _ in range(100):
        try:
            if call("GET", "/healthz")[0] == 200:
                break
        except OSError:
            pass
        time.sleep(0.2)
time.sleep(1.5)
stop.set()
[t.join() for t in ts]
rows = dict(l.split("|") for l in psql(f"SELECT idempotency_key,id FROM bookings WHERE event_id='{eid}'").split("\n"))
lost = [k for k in acked if k not in rows]
changed = [k for k in acked if k in rows and rows[k] != acked[k]]
avail = int(psql(f"SELECT available FROM events WHERE id='{eid}'"))
active = int(psql(f"SELECT COALESCE(SUM(quantity),0) FROM bookings WHERE event_id='{eid}' AND status='active'"))
print(f"statuses={statuses} acked={len(acked)} rows={len(rows)} lost={len(lost)} changed_id={len(changed)} "
      f"available={avail} active={active} capacity={cap} invariant={avail + active == cap}")
sys.exit(0 if not lost and not changed and avail + active == cap else 1)
````

### probe.py: Сырые HTTP-запросы

Команда: `python3 probe.py 18097`

````python
#!/usr/bin/env python3
"""Raw-socket API probes against the reviewer's own local server.  Usage: probe.py <port>"""
import json, socket, sys, time, uuid

PORT = int(sys.argv[1])


def raw(data, timeout=25, half_close=False):
    s = socket.create_connection(("127.0.0.1", PORT), timeout=timeout)
    s.sendall(data)
    out = b""
    try:
        while True:
            c = s.recv(65536)
            if not c:
                break
            out += c
    except (socket.timeout, ConnectionResetError):
        pass
    s.close()
    return out


def req(method, path, body=b"", headers=None):
    headers = dict(headers or {})
    if isinstance(body, str):
        body = body.encode()
    h = "".join(f"{k}: {v}\r\n" for k, v in headers.items())
    cl = f"Content-Length: {len(body)}\r\n" if method != "GET" or body else ""
    return raw(f"{method} {path} HTTP/1.1\r\nHost: x\r\nConnection: close\r\n{cl}{h}\r\n".encode() + body)


def show(label, r, n=220):
    head, _, body = r.partition(b"\r\n\r\n")
    lines = head.decode("latin1").split("\r\n")
    ctype = next((l for l in lines if l.lower().startswith("content-type")), "-")
    print(f"{label:<44} {lines[0] if lines else 'EMPTY'} | {ctype} | {body[:n].decode('utf8', 'replace').strip()}")


J = {"Content-Type": "application/json"}


def ev(body):
    return req("POST", "/events", body, J)


r = ev('{"name":"base","capacity":10}')
eid = json.loads(r.partition(b"\r\n\r\n")[2])["id"]
print("event", eid)

print("\n-- SQL injection payloads (expect stored verbatim / rejected, tables intact)")
show("name injection", ev(json.dumps({"name": "x'); DROP TABLE bookings;--", "capacity": 1})))
show("id injection", req("GET", "/events/1'%20OR%20'1'='1"))
show("uuid-shaped injection", req("GET", "/events/" + "'" * 36))
show("event_id injection", req("POST", "/bookings", json.dumps({"event_id": f"{eid}' OR '1'='1", "quantity": 1}), {**J, "Idempotency-Key": "inj1"}))
show("key injection", req("POST", "/bookings", json.dumps({"event_id": eid, "quantity": 1}), {**J, "Idempotency-Key": "a';DROP TABLE events;--"}))
show("key newline", req("POST", "/bookings", json.dumps({"event_id": eid, "quantity": 1}), {**J, "Idempotency-Key": "abc%0A"}))

print("\n-- JSON edge cases")
show("lone surrogate escape", ev(r'{"name":"\ud800","capacity":1}'))
show("surrogate pair", ev(r'{"name":"😀","capacity":1}'))
show("uppercase field names", ev('{"NAME":"upper","CAPACITY":3}'))
show("duplicate key last wins", ev('{"name":"dup","capacity":1,"capacity":7}'))
show("control chars in name", ev('{"name":"a\\u0007\\u001b[31mb\\nc","capacity":1}'))
show("zero-width only name", ev('{"name":"\\u200b","capacity":1}'))
show("name 200 emoji", ev(json.dumps({"name": "😀" * 200, "capacity": 1}, ensure_ascii=False)))
show("capacity as string", ev('{"name":"a","capacity":"5"}'))
show("capacity 1e3", ev('{"name":"a","capacity":1e3}'))
show("capacity 5.0", ev('{"name":"a","capacity":5.0}'))
show("capacity null", ev('{"name":"a","capacity":null}'))
show("deep nesting 8000", ev("[" * 8000 + "]" * 8000))
show("deep nesting in unknown field", ev('{"x":' + "[" * 8000 + "]" * 8000 + '}'))
show("BOM", ev(b'\xef\xbb\xbf{"name":"a","capacity":1}'))
show("ctype charset=latin1", req("POST", "/events", '{"name":"cs","capacity":1}', {"Content-Type": "application/json; charset=latin1"}))
show("ctype uppercase", req("POST", "/events", '{"name":"up","capacity":1}', {"Content-Type": "APPLICATION/JSON"}))
show("ctype malformed", req("POST", "/events", '{"name":"mal","capacity":1}', {"Content-Type": "application/json;;;="}))

print("\n-- routing / non-JSON responses")
show("double slash GET", req("GET", "//healthz"))
show("dotdot GET", req("GET", f"/events/../events/{eid}"))
show("double slash POST", req("POST", "//events", '{"name":"r","capacity":1}', J))
show("trailing slash", req("GET", f"/events/{eid}/"))
show("OPTIONS", req("OPTIONS", "/events"))
show("HEAD healthz", req("HEAD", "/healthz"))
show("uppercase uuid path", req("GET", f"/events/{eid.upper()}"))
show("encoded slash", req("GET", f"/events/a%2Fb"))
show("nil uuid", req("GET", "/events/00000000-0000-0000-0000-000000000000"))
show("malformed request line", raw(b"GARBAGE\r\n\r\n"))
show("host missing", raw(b"GET /healthz HTTP/1.1\r\nConnection: close\r\n\r\n"))
show("oversized header (64 KiB)", req("GET", "/healthz", b"", {"X-Big": "a" * 65536}))
show("16 KiB exactly header", req("GET", "/healthz", b"", {"X-Big": "a" * 16000}))

print("\n-- size limits")
show("body 16 KiB + 1", ev(b" " * (16 * 1024 + 1)))
show("body exactly 16 KiB valid", ev(b'{"name":"sz","capacity":1}' + b" " * (16 * 1024 - 26)))
show("chunked over limit", raw(b"POST /events HTTP/1.1\r\nHost: x\r\nConnection: close\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n" + b"5000\r\n" + b" " * 0x5000 + b"\r\n0\r\n\r\n"))
show("cancel body 1 byte", req("POST", f"/bookings/{uuid.uuid4()}/cancel", b"x"))
show("cancel body big", req("POST", f"/bookings/{uuid.uuid4()}/cancel", b"x" * 100000))
show("cancel whitespace body", req("POST", f"/bookings/{uuid.uuid4()}/cancel", b" "))
show("key 128 ok", req("POST", "/bookings", json.dumps({"event_id": eid, "quantity": 1}), {**J, "Idempotency-Key": "k" * 128}))
show("key 129", req("POST", "/bookings", json.dumps({"event_id": eid, "quantity": 1}), {**J, "Idempotency-Key": "k" * 129}))
show("quantity > capacity", req("POST", "/bookings", json.dumps({"event_id": eid, "quantity": 11}), {**J, "Idempotency-Key": "toobig"}))

print("\n-- slow clients")
t = time.time()
s = socket.create_connection(("127.0.0.1", PORT), timeout=30)
s.sendall(b"GET /healthz HTTP/1.1\r\nHost: x\r\nX-Slow: ")
try:
    d = s.recv(4096)
except OSError as e:
    d = repr(e).encode()
print(f"slow header closed after {time.time() - t:.1f}s: {d[:60]!r}")
s.close()
t = time.time()
s = socket.create_connection(("127.0.0.1", PORT), timeout=30)
s.sendall(b"POST /events HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{")
try:
    d = s.recv(4096)
except OSError as e:
    d = repr(e).encode()
print(f"stalled body answered after {time.time() - t:.1f}s: {d[:40]!r} .. {d.partition(bytes([13, 10, 13, 10]))[2][:120]!r}")
s.close()
````

### faults.sh: Отказы БД, блокировки и обрыв клиента

Команда: `bash faults.sh "$S/server" "$S/faults.log"`

````bash
#!/bin/bash
# Fault injection against one real server process and a reviewer-owned PostgreSQL container.
# Usage: faults.sh <server-binary> <log-file>
BIN=$1; LOG=$2; C=claude-review-faults; B=127.0.0.1:18098
docker run -d --name $C -e POSTGRES_USER=rev -e POSTGRES_PASSWORD=rev -e POSTGRES_DB=rev -p 127.0.0.1::5432 \
  --tmpfs /var/lib/postgresql/data postgres:17-alpine >/dev/null
trap 'docker rm -f $C >/dev/null 2>&1; kill $PID 2>/dev/null' EXIT
for _ in $(seq 1 40); do docker exec $C psql -h 127.0.0.1 -U rev -d rev -c 'select 1' >/dev/null 2>&1 && break; sleep 1; done
A=$(docker port $C 5432 | head -1)
DATABASE_URL="postgres://rev:rev@$A/rev?sslmode=disable" HTTP_ADDR=$B "$BIN" >"$LOG" 2>&1 & PID=$!
sleep 1
J='Content-Type: application/json'
mk(){ curl -s -X POST $B/events -H "$J" -d "{\"name\":\"$1\",\"capacity\":$2}" | python3 -c 'import json,sys;print(json.load(sys.stdin)["id"])'; }
bk(){ curl -s -o /dev/null -w "$1 code=%{http_code} t=%{time_total}\n" --max-time "${3:-25}" -X POST $B/bookings -H "$J" -H "Idempotency-Key: $1" -d "{\"event_id\":\"$2\",\"quantity\":1}"; }
lock(){ docker exec $C psql -U rev -d rev -c "begin; select 1 from events where id='$1' for update; select pg_sleep($2); commit;" >/dev/null; }
errs(){ grep -c '"level":"ERROR"' "$LOG"; }
HOT=$(mk hot 50); OTHER=$(mk other 50)

echo "--- A. row locked 12s: timeout, safe retry, no ghost backends, nothing logged"
lock "$HOT" 12 & L=$!; sleep 0.5
bk a1 "$HOT"
docker exec $C psql -U rev -d rev -Atc "select 'waiting_on_lock='||count(*) from pg_stat_activity where wait_event_type='Lock'"
echo "ERROR log lines after 503: $(errs)"
wait $L; bk a1 "$HOT"; bk a1 "$HOT"
docker exec $C psql -U rev -d rev -Atc "select 'a1 rows='||count(*) from bookings where idempotency_key='a1'"

echo "--- B. client disconnects while waiting on the lock"
lock "$HOT" 3 & L=$!; sleep 0.5
bk b1 "$HOT" 1; sleep 0.3
grep '"level":"ERROR"' "$LOG" | cut -c1-140; wait $L
docker exec $C psql -U rev -d rev -Atc "select 'b1 rows='||count(*) from bookings where idempotency_key='b1'"

echo "--- C. default pool saturated by requests to one locked event (pool=max(4,NumCPU), NumCPU=$(nproc))"
lock "$HOT" 12 & L=$!; sleep 0.5
for i in 1 2 3 4; do bk "c$i" "$HOT" 20 & done; sleep 1
bk c-other "$OTHER" 20 &
curl -s -o /dev/null -w 'healthz code=%{http_code} t=%{time_total}\n' --max-time 20 $B/healthz
wait $(jobs -p | grep -v "^$PID$");
docker exec $C psql -U rev -d rev -Atc "select e.name, e.available, e.capacity, coalesce(sum(b.quantity) filter (where b.status='active'),0) from events e left join bookings b on b.event_id=e.id group by e.id order by 1"

echo "--- D. frozen database (docker pause)"
before=$(errs); docker pause $C >/dev/null
bk d1 "$OTHER"
curl -s -w ' GET event code=%{http_code} t=%{time_total}\n' --max-time 25 $B/events/$OTHER
curl -s -w ' healthz code=%{http_code} t=%{time_total}\n' --max-time 25 $B/healthz
docker unpause $C >/dev/null; sleep 1
echo "new ERROR log lines for three failures: $(( $(errs) - before ))"
bk d1 "$OTHER"

echo "--- E. database stopped"
docker stop -t 3 $C >/dev/null
bk e1 "$OTHER" 5; bk e1 "$OTHER" 5
curl -s -w ' POST /events code=%{http_code}\n' -X POST $B/events -H "$J" -d '{"name":"x","capacity":1}'
curl -s -w ' healthz code=%{http_code}\n' $B/healthz
echo "secret in log: $(grep -c 'rev:rev' "$LOG")"
grep '"level":"ERROR"' "$LOG" | tail -2 | cut -c1-200
kill -TERM $PID; wait $PID; echo "server SIGTERM rc=$?"
````

### healthrace.sh: Ложная готовность PostgreSQL

Команда: `bash healthrace.sh 15 "$S/init"`

````bash
#!/bin/bash
# 1) Natural frequency: right after `up --wait` of the project's compose.test.yaml, does TCP accept a PG handshake?
# 2) Mechanism: same healthcheck command, slow init script, in a reviewer-owned container.
cd /home/petr/review/project || exit 1
CHECK='import socket,struct,sys
h,p=sys.argv[1].rsplit(":",1)
try:
    s=socket.create_connection((h,int(p)),timeout=2); s.sendall(struct.pack("!ii",8,80877103)); d=s.recv(1)
    print("ok" if d in (b"S",b"N") else "bad:%r"%d)
except Exception as e: print("FAIL:"+type(e).__name__)'
ok=0; fail=0
for i in $(seq 1 "${1:-15}"); do
  p="claude-review-hc-$i"
  docker compose -f compose.test.yaml -p "$p" up -d --wait --wait-timeout 60 db >/dev/null 2>&1
  addr=$(docker compose -f compose.test.yaml -p "$p" port db 5432)
  r=$(python3 -c "$CHECK" "$addr")
  [ "$r" = ok ] && ok=$((ok+1)) || { fail=$((fail+1)); echo "run $i: $r"; }
  docker compose -f compose.test.yaml -p "$p" down --volumes >/dev/null 2>&1
done
echo "natural: handshake ok=$ok fail=$fail"

mkdir -p "$2"; printf '#!/bin/sh\nsleep 6\n' > "$2/slow.sh"; chmod 755 "$2/slow.sh"
docker run -d --name claude-review-hc-mech -e POSTGRES_USER=booking -e POSTGRES_PASSWORD=booking -e POSTGRES_DB=booking \
  -v "$2/slow.sh:/docker-entrypoint-initdb.d/slow.sh:ro" --tmpfs /var/lib/postgresql/data postgres:17-alpine >/dev/null
for i in $(seq 1 24); do
  u=$(docker exec claude-review-hc-mech sh -c 'pg_isready -U booking -d booking >/dev/null 2>&1; echo $?')
  t=$(docker exec claude-review-hc-mech sh -c 'pg_isready -h 127.0.0.1 -U booking -d booking >/dev/null 2>&1; echo $?')
  echo "sample=$i healthcheck_cmd_rc=$u tcp_rc=$t"
  sleep 0.4
done | awk '{k=$2" "$3; if(k!=p){print; p=k}}'
docker rm -f claude-review-hc-mech >/dev/null
````
