# Независимое техническое ревью

Дата: 7 ноября 2026 года (Asia/Singapore). Проверен commit `d819117aa8657cfba0831e031b3b14e9f6867ee9`.
Основание: `../REVIEW_INSTRUCTIONS.md`, `TASK.md`, `README.md` и все исходники, тесты и файлы сборки проекта. Реализация, тесты, зависимости и документация проекта не изменялись. Собственные проверки и их журналы находятся в `/tmp/booking-review-wU8Ttp`; единственный добавленный файл проекта — этот отчёт. Другие агенты не использовались.

## Заключение о соответствии TASK.md

Основные требования выполнены в проверенных сценариях. Сервис собирается, запускается через Docker/Compose, предоставляет описанный JSON API и работает с настоящей PostgreSQL. Данные и ключи идемпотентности сохраняются после перезапуска приложений и PostgreSQL. Два отдельных процесса с общей БД выдержали конкурентные бронирования, повторы одного ключа, конфликтующие параметры ключа, одновременные отмены и смешанные создание/отмену без перепродажи или расхождения счётчика с активными бронями. Начальная схема создаётся при одновременном запуске и повторно применяется к данным без их потери.

Подтверждён один дефект P2 в ограничении времени операций и остановке при задержке БД. Это проблема устойчивости; нарушения сохранности, идемпотентности или возврата мест в выполненных проверках не обнаружены. Подтверждённых P0/P1 нет. Отсутствие авторизации соответствует заданию и дефектом не считается. Результаты сканера зависимостей ниже отделены от подтверждённых уязвимостей приложения.

## Подтверждённый дефект

### P2 — долгое ожидание БД не ограничено HTTP WriteTimeout и задерживает завершение процесса

**Места:** `cmd/booking/main.go:49`, `cmd/booking/main.go:58–65`, `cmd/booking/main.go:73–77`; передача исходного контекста запроса в `internal/booking/handler.go:144` и ожидание блокировки в `internal/booking/store.go:125–153`. Аналогично контексты без собственного deadline используются другими обработчиками.

**Условие:** PostgreSQL долго не освобождает блокировку строки события, например из-за зависшей транзакции обслуживания; клиент отправляет бронирование и оставляет соединение открытым. Воспроизведение удерживало ровно одну строку в собственной тестовой БД и отправляло один запрос, без нагрузочной атаки.

**Причина и последствия:** `WriteTimeout=30s` ограничивает запись в HTTP-соединение, но не задаёт deadline для `r.Context()` и PostgreSQL-запроса. Пока обработчик не дошёл до записи ответа, он продолжает ждать и удерживает соединение пула/транзакцию, включая advisory lock ключа. При нескольких таких ожиданиях может заполниться пул из 20 соединений, после чего будут ждать другие запросы, включая `/healthz` (полное заполнение пула отдельно не испытывалось). При SIGTERM `Shutdown` ждёт 10 секунд и возвращает ошибку, но отложенный `pool.Close()` ждёт занятые соединения; нет принудительного закрытия активных HTTP-запросов или отмены их контекстов после исчерпания периода завершения. Остановка процесса поэтому тоже не ограничена этими 10 секундами.

**Доказательство:** самостоятельный `timeout.py` приведён полностью в приложении. На 32-й секунде в `pg_stat_activity` оставался запрос `SELECT capacity, booked_seats ... FOR UPDATE` с `wait_event_type='Lock'`, клиент всё ещё ждал. После SIGTERM процесс оставался жив ещё через 12 секунд. После освобождения блокировки бронь была зафиксирована, клиент получил `RemoteDisconnected`, процесс завершился с кодом 1. Наблюдение фиксации после тайм-аута само по себе не означает нарушения идемпотентности: повтор по ключу остаётся безопасным. Дефект — отсутствие ограничения времени удержания ресурсов и завершения процесса.

Фактический вывод:

```text
CONFIRMED: at 32 seconds request still waits for DB lock beyond WriteTimeout=30s
CONFIRMED: process still alive 12 seconds after SIGTERM despite Shutdown timeout=10s
HTTP result after lock release: ('RemoteDisconnected', 'Remote end closed connection without response')
Process exit after lock release: 1
Bookings committed after timeout and SIGTERM: 1
```

**Направление исправления:** установить явный бюджет времени для обработки/операций БД и корректно отменять контексты; предусмотреть отмену оставшихся запросов и принудительное закрытие соединений после периода graceful shutdown, до ожидания `pool.Close`. При необходимости дополнительно ограничить ожидание SQL/блокировок на стороне PostgreSQL. Конкретная длительность тайм-аутов — эксплуатационное решение, задание её не предписывает.

## Архитектура

**Соразмерность.** Для небольшой задачи структура достаточна: `cmd/booking/main.go` собирает зависимости и управляет жизненным циклом; `handler.go` занимается HTTP, JSON и валидацией; `store.go` содержит операции и транзакции; `migrate.go` — схему; `types.go` — общие модели и ошибки. Нет дополнительной инфраструктуры или набора абстракций, стоимость которых превышала бы задачу. Добавление обязательного service/repository слоя само по себе здесь не дало бы исправления обнаруженных проблем.

**Зависимости и связанность.** Направление вызовов понятно: main → handler → store → pgx/PostgreSQL. Store не импортирует HTTP, а обработчик преобразует типизированные ошибки (`handler.go:187–200`). Привязка обработчика к конкретному `*Store` (`handler.go:15–17`) делает изолированные HTTP-тесты без БД неудобными, но позволяет проверять реальные транзакционные гарантии без имитации PostgreSQL. Это разумный компромисс для текущего объёма; узкий интерфейс полезен при появлении второй реализации или необходимости независимых тестов транспорта, а не как обязательный паттерн.

**Размещение инвариантов и транзакций.** Критические гарантии сосредоточены в Store и БД, поэтому действуют между процессами. В `store.go:125–185` advisory lock берётся до проверки ключа и блокировки события; существующая бронь проверяется до доступности, что позволяет повторять запрос при уже исчерпанной вместимости. `SELECT ... FOR UPDATE`, увеличение счётчика и вставка брони находятся в одной транзакции. В `store.go:198–228` условный перевод active → cancelled и уменьшение счётчика также атомарны; только изменивший статус запрос освобождает места. Ни process-local mutex, ни память экземпляра не участвуют в этих гарантиях.

Денормализованный `events.booked_seats` делает чтение доступности дешёвым. Цена — обязанность обновлять счётчик при каждом будущем пути изменения брони. `CHECK` в `migrate.go:13–14` ограничивает диапазон счётчика, но не доказывает равенство сумме активных броней; сейчас равенство поддерживается транзакционным кодом, и независимая SQL-проверка его подтвердила. Прямые произвольные изменения БД считаются доверенными административными действиями, а не поддерживаемым API.

**Валидация и изменения.** Пользовательские ограничения находятся на HTTP-границе (`handler.go:61–68`, `131–142`), базовые ограничения повторены в схеме. Store не является самостоятельным валидирующим доменным API: например, верхние границы capacity/seats и байтовая длина ключа целиком обеспечиваются обработчиком. Сейчас других производственных вызывающих сторон нет, поэтому это не дефект; при добавлении очереди/CLI следует переиспользовать валидацию, чтобы не получить разные правила для разных входов.

**Понятность и поддержка.** Операции короткие, SQL параметризован, правила повторов и отмены описаны в README. Простая миграция `CREATE IF NOT EXISTS` под advisory lock достаточна для начальной схемы; она не обновит уже существующую таблицу при будущем изменении DDL, что честно указано в README. Отсутствие версионированных миграций сейчас не нарушение задания. Наблюдаемость упрощена сильнее: `internalError` игнорирует причину (`handler.go:250–251`), а main логирует лишь жизненный цикл. Это защищает HTTP-ответы от утечек, но усложняет поиск причины реального 500; серверный журнал ошибки с идентификатором запроса был бы полезным необязательным улучшением.

Подтверждённое неудачное упрощение — управление временем выполнения и завершением процесса, описанное в P2 выше. Избыточной архитектурной сложности, требующей устранения для этой задачи, не обнаружено.

## Безопасность

### Модель угроз и границы доверия

Предполагается небольшой сервис в доверенной среде, где любой допущенный к HTTP клиент вправе создавать события, бронировать, читать и отменять любые брони. HTTP-тело, путь и заголовки недоверенные. PostgreSQL, DNS/сеть до него и конфигурация `DATABASE_URL`/`HTTP_ADDR` контролируются оператором. Доступ к БД с правами администратора даёт возможность нарушить данные вне API; защита от такого администратора в задание не входит. Проверки выполнялись только против собственных локальных процессов и контейнеров в VM.

### Проверенные меры и наблюдения

- **SQL-инъекции:** значения передаются параметрами `$1…$4`, SQL не строится из названия, ключа или UUID (`store.go:27–30`, `133–179`, `198–220`). Динамический DDL в тестах использует `pgx.Identifier.Sanitize()` (`integration_test.go:46`). Строка `'); DROP TABLE events; --` была успешно сохранена как название и ключ; таблицы продолжили работать. Эксплуатируемая SQL-инъекция в текущих запросах не подтверждена.
- **Недоверенный JSON и размеры:** проверяются media type, неизвестные поля, второе JSON-значение, типы чисел; тело ограничено 1 МиБ (`handler.go:211–229`). Отдельно ограничены title, capacity, seats, ключ, исключён NUL. Независимые запросы с пустым/битым JSON, null/массивом, лишним полем, дробным/переполненным числом, выходом за диапазон, слишком длинными строками и NUL отклонены. Лимит title считается в Unicode code points, ключа — в байтах, что соответствует README. Заголовки ограничиваются стандартным лимитом `net/http`, собственный меньший лимит не задан. Политика отклонения повторяющихся имён полей/некорректного UTF-8 поверх поведения `encoding/json` не реализована; отдельное требование такой политики в TASK.md отсутствует.
- **Время и ресурсы:** есть ReadHeaderTimeout 5s, ReadTimeout 15s, WriteTimeout 30s, IdleTimeout 60s, максимум 20 соединений пула, бюджет запуска 30s. При этом WriteTimeout не ограничивает ожидание БД — подтверждённый P2. Rate limiting и общий лимит числа обработчиков отсутствуют; публичное размещение без внешних ограничений повышает риск исчерпания ресурсов. Тяжёлая нагрузка не проводилась.
- **Ошибки и секреты:** неожиданные ошибки Store возвращаются как общий `internal server error`; SQL, DSN и пароль в HTTP-ответы по коду не включаются. Ошибки JSON раскрывают лишь контекст разбора/тип поля и экранируются JSON encoder. При локальных проверках ошибочного DSN с тестовым паролем pgx маскировал пароль в startup-логе. Это выборочная проверка, а не доказательство отсутствия утечек во всех форматах DSN. Внутренние ошибки запросов вообще не логируются; наблюдаемость можно улучшить, сохранив редактирование секретов.
- **Контейнеры и сеть:** финальный образ запускается пользователем `app`, не root (`Dockerfile:9–12`), содержит только бинарник из build-stage. Compose сохраняет БД в именованном томе и ждёт healthcheck PostgreSQL. Фактически оба сервиса стали healthy. В исходном `compose.yaml:5–9,22–25` демонстрационный пароль `booking`, PostgreSQL и HTTP опубликованы на всех интерфейсах, `sslmode=disable`; это опасные настройки для доступной посторонним сети. Для локальной демонстрации они описаны в README. Для публичного размещения нужны секреты оператора, ограничение доступа к БД/портам, надлежащие роли и защищённый транспорт. Авторизация и TLS — отдельные условия такого развёртывания, а не недостающие требования TASK.md. В собственном стенде порты публиковались только на loopback.

### Проверка известных уязвимостей зависимостей

Использован **govulncheck v1.9.0**, база `https://vuln.go.dev`, сообщённая дата обновления базы `2026-11-05 23:49:57 UTC`. Проверялись исходники на Go 1.27.3, исходники с `GOTOOLCHAIN=go1.24.13` и бинарник, действительно извлечённый из Docker-образа. `go version -m` подтвердил, что Dockerfile собрал **Go 1.24.13**; зависимости соответствуют go.mod/go.sum. Модули проверены `go mod verify`.

| Режим | Фактический результат |
| --- | --- |
| Source, Go 1.27.3 | exit 3: 3 symbol-level сообщения из 2 модулей, дополнительно 2 package-level и 20 module-level |
| Source, Go 1.24.13 | exit 3: 24 symbol-level сообщения из 2 модулей и stdlib, дополнительно 13 package-level и 31 module-level |
| Docker binary, Go 1.24.13, stripped | exit 3: 67 сообщений; детализация грубее исходного анализа, среди них есть неиспользуемые пакеты |

Формулировка сканера «affected» означает статическую достижимость символа/наличие версии, а не подтверждённую эксплуатацию конкретного HTTP API. В частности, вызовы через интерфейсы дают широкий граф; количество 67 нельзя трактовать как 67 доказанных уязвимостей сервиса.

| Срабатывание / группа | Применимость к текущему коду и статус |
| --- | --- |
| [GO-2026-5004](https://pkg.go.dev/vuln/GO-2026-5004), pgx 5.7.6; исправление 5.9.2 | Требует simple protocol и SQL с dollar-quoted литералом, содержащим похожий на placeholder текст. Здесь default protocol и нет таких литералов. Условия не найдены; подтверждённой SQL-инъекцией не считается. |
| [GO-2026-6629](https://pkg.go.dev/vuln/GO-2026-6629), x/text 0.24.0; исправление 0.41.0 | Описание касается профиля Nickname и короткого выходного буфера. pgx использует `precis.OpaqueString.Bytes` для доверенного пароля БД (`pgconn/auth_scram.go:141–143`), HTTP-строки туда не передаются. Условия эксплуатации через API не подтверждены. |
| [GO-2026-5970](https://pkg.go.dev/vuln/GO-2026-5970), x/text; исправление 0.39.0 | Проблема нормализации некорректного UTF-8. Найденный путь также связан с конфигурацией SCRAM-пароля, а не title/idempotency_key. Риск зависит от входов оператора; отдельного PoC не было. |
| [GO-2026-4771](https://pkg.go.dev/vuln/GO-2026-4771), [GO-2026-4772](https://pkg.go.dev/vuln/GO-2026-4772), pgx; исправление 5.9.0 | Source-скан относит их к package-level, уязвимые `Backend.Receive`/декодеры серверной стороны PostgreSQL-протокола приложением не вызываются. Не подтверждены как уязвимости этого клиента PostgreSQL. |
| Stdlib HTTP/2: GO-2026-6617/6612/6611/6603/6089; TLS/x509: GO-2026-6809/6607/6090/5856/5037/4947/4946/4870 | Docker runtime старее исправлений. Приложение запускает HTTP/1 через ListenAndServe, без TLS/h2c; compose использует `sslmode=disable`. В данном запуске предпосылки большей части этих сообщений отсутствуют. При включении TLS к БД следует отдельно оценить сертификаты/доверие и обновить runtime. |
| GO-2026-6613, успешный CONNECT | Все прикладные маршруты проверяют метод, CONNECT не получает 2xx; требуемое условие не найдено. |
| GO-2026-6608 (MIME), 6088 (XML), 5972 (ASN.1), 5039 (textproto), 4601 (IPv6 URL) | Статический граф включает универсальные возможности net/http/pgx. Multipart/XML не используются как вход API, ASN.1-сертификаты не разбираются в compose. Для textproto/IPv6 конкретное влияние на приложение не воспроизведено. Это кандидаты для анализа/обновления runtime, а не подтверждённые здесь атаки. |
| GO-2026-4971 (Windows), GO-2026-4602 (os.Root); SSH/OpenPGP, шаблоны, tar и прочие сообщения уровня пакета/модуля | Linux-бинарник не использует соответствующие прикладные возможности; наличие зависимости или стандартного пакета не означает доступный путь атаки. |

Прямо прочитаны официальные карточки первых пяти перечисленных Go advisory; готовые решения проекта не искались. Полные журналы сканирования сохранены во временной папке. Практическое направление — обновление сборочного Go и зависимостей с повторной проверкой применимости. Отдельная эксплуатация advisory не проводилась. **Подтверждённой SQL-инъекции, утечки секрета или иной самостоятельной уязвимости по этим сообщениям нет; доказанным отсутствием уязвимостей это не является.**

## Неподтверждённые риски и необязательные рекомендации

1. При публичном HTTP-доступе без ограничений частоты клиент может неограниченно создавать события и брони, раздувая бессрочно хранимые таблицы. Предпосылка — такое развёртывание и длительный поток запросов; исчерпание диска не воспроизводилось. Ограничения доступа/квоты/наблюдение — эксплуатационные меры сверх задания.
2. Публикация PostgreSQL со стандартным паролем и ролью, создаваемой POSTGRES_USER, даёт доступному по сети постороннему широкие права на БД. Это подтверждаемая конфигурация демонстрационного стенда, но доступность из внешней сети и фактическая компрометация не проверялись. Не переносить демонстрационные настройки в публичную среду.
3. Обновить версии runtime и библиотек с учётом результатов govulncheck. Применимость описана выше; не все срабатывания являются проблемами текущего кода.
4. Добавить диагностическое логирование внутренних ошибок с очисткой секретов и request ID; обязательного требования к этому нет.
5. При дальнейшем развитии предусмотреть версионированные миграции и общий валидатор для новых транспортов. Текущая схема и единственный HTTP-вход не требуют дополнительных слоёв сейчас.

## Качество автоматических тестов

Все пять штатных PostgreSQL-тестов прошли с `-race -count=1` на Go 1.27.3 и Go 1.24.13. Они используют настоящую БД и уникальные схемы с адресной очисткой, проверяют число успешных конкурентных запросов, отсутствие дубликатов, конфликт параметров ключа, возврат счётчика и неизменность cancelled_at. Это содержательные проверки инвариантов, а не только статусов ответа.

Ограничения набора: без TEST_DATABASE_URL пропускаются **все пять тестов**, даже HTTP-валидация; README это предупреждает. Штатное моделирование двух экземпляров использует два пула в одном процессе; мои проверки дополнительно использовали два самостоятельных HTTP-процесса. `TestMigrationAndPersistence` закрывает/открывает пул, но повторная миграция fixture выполняется до записи события — этим тестом отдельно не проверяется миграция с существующими данными. Одновременная первая миграция, фактический рестарт процессов/БД и повторная миграция с данными проверены независимым скриптом.

Покрытие штатного набора: `booking/internal/booking` — **67.1%**, `cmd/booking` — **0%**, суммарно **57.7%** по `go tool cover -func`. Основные пропуски — ветви ошибок инфраструктуры, health/startup/shutdown и часть валидации. Не требуется добиваться процента ради процента; найденный P2 показывает полезность целевого теста отмены ожидания и завершения.

## Фактически выполненные команды и результаты

Рабочая папка для следующих команд — `/home/petr/review/project`. Указанные пароли являются исключительно созданными для ревью локальными тестовыми значениями.

### Окружение, статические проверки и штатные тесты

```bash
go version
docker version
docker compose version
go test ./...
go test -v ./...
go vet ./...
go mod verify
docker compose config --quiet
```

Результаты: Go 1.27.3, Docker 29.9.2, Compose 5.7.0. Компиляция/вет/проверка сумм/Compose config успешны. Оба запуска тестов без переменной БД успешны с пропуском всех пяти интеграционных тестов; это не засчитано как проверка бизнес-сценариев.

```bash
mktemp -d /tmp/booking-review-XXXXXX
# Получено /tmp/booking-review-wU8Ttp
docker run -d --name booking-review-wU8Ttp-pg \
  -e POSTGRES_USER=booking -e POSTGRES_PASSWORD=review-local-password \
  -e POSTGRES_DB=booking -p 127.0.0.1::5432 postgres:16-alpine
docker exec booking-review-wU8Ttp-pg pg_isready -U booking -d booking
docker port booking-review-wU8Ttp-pg 5432
docker exec booking-review-wU8Ttp-pg psql -U booking -d booking -Atc 'select version()'
go build -o /tmp/booking-review-wU8Ttp/booking ./cmd/booking
```

Успешно: PostgreSQL **16.15**, собственный контейнер с собственным томом, локальная публикация порта. Изначально порт был 32768; после restart Docker назначал новый динамический порт, поэтому дальнейшие команды/скрипты читают `docker port` заново.

```bash
TEST_DATABASE_URL='postgres://booking:review-local-password@127.0.0.1:32768/booking?sslmode=disable' \
  go test -race -count=1 -v -coverprofile=/tmp/booking-review-wU8Ttp/coverage.out ./...
```

Первый такой запуск не прошёл: во время компиляции тестов собственный параллельный скрипт перезапустил БД, а динамический опубликованный порт изменился. Ошибки — `connect: connection refused`. Первая версия независимого скрипта по той же причине не прошла этап повторного подключения. Это ошибка организации стенда ревью, **не дефект приложения**. После исправления только временного скрипта проверки повторены с актуальным портом:

```bash
TEST_DATABASE_URL="postgres://booking:review-local-password@$(docker port booking-review-wU8Ttp-pg 5432)/booking?sslmode=disable" \
  go test -race -count=1 -v -coverprofile=/tmp/booking-review-wU8Ttp/coverage.out ./...
go tool cover -func=/tmp/booking-review-wU8Ttp/coverage.out
GOTOOLCHAIN=go1.24.13 \
TEST_DATABASE_URL="postgres://booking:review-local-password@$(docker port booking-review-wU8Ttp-pg 5432)/booking?sslmode=disable" \
  go test -race -count=1 ./...
```

Оба полных запуска успешны: пять тестов PASS, race detector сообщений не выдал. На Go 1.27.3 пакет прошёл за 2.120s, на Go 1.24.13 — 2.293s (включая работу race runtime).

### Независимые проверки

```bash
python3 /tmp/booking-review-wU8Ttp/check.py
python3 /tmp/booking-review-wU8Ttp/timeout.py
```

Полный текст обоих скриптов ниже. Последняя версия `check.py` успешно завершилась с кодом 0, все шесть групп PASS. Повторный запуск создаёт новую отдельную схему. `timeout.py` завершился с кодом 0 и подтвердил описанный P2 (его дочернее приложение ожидаемо завершилось с кодом 1). После последнего изменения временного `check.py` уточнена SQL-проверка инварианта, чтобы она читала именно выделенную схему; этот вариант также выполнен успешно.

### Docker/Compose и секреты в ошибках запуска

```bash
docker build -t booking-review-wu8ttp:local .
docker compose -p booking-review-wu8ttp -f compose.yaml \
  -f /tmp/booking-review-wU8Ttp/compose.override.yaml up -d --no-build --wait
docker compose -p booking-review-wu8ttp -f compose.yaml \
  -f /tmp/booking-review-wU8Ttp/compose.override.yaml ps
docker inspect booking-review-wu8ttp-app-1 --format '{{.Config.User}} {{.State.Health.Status}}'
curl -fsS http://127.0.0.1:32772/healthz
docker compose -p booking-review-wu8ttp -f compose.yaml \
  -f /tmp/booking-review-wU8Ttp/compose.override.yaml logs --no-color app
```

Сборка успешна, оба контейнера healthy, пользователь приложения `app`, `/healthz` → `{"status":"ok"}`. Compose использован с исходными настройками приложения/БД и отдельным именем проекта; override меняет только адреса/динамические порты публикации и задаёт имя предварительно собранного образа. Полный текст override:

```yaml
services:
  postgres:
    ports: !override
      - "127.0.0.1::5432"
  app:
    image: booking-review-wu8ttp:local
    ports: !override
      - "127.0.0.1::8080"
```

```bash
env DATABASE_URL='postgres://booking:REVIEW_SECRET@localhost:invalid/booking' /tmp/booking-review-wU8Ttp/booking
env DATABASE_URL='postgres://booking:REVIEW_SECRET@localhost/booking?bad=%ZZ' /tmp/booking-review-wU8Ttp/booking
env DATABASE_URL='postgres://booking:REVIEW_SECRET@localhost/%ZZ' /tmp/booking-review-wU8Ttp/booking
```

Завершение с кодом 1 ожидаемо. Невалидный порт/путь дали ошибки разбора с замаскированным паролем; неизвестный query-параметр игнорируется pgx, после чего подключение к неиспользуемому localhost:5432 отказано, пароль не выведен. Это тест обработки ошибок на собственном окружении.

### Сканирование зависимостей и бинарника

```bash
GOBIN=/tmp/booking-review-wU8Ttp/bin go install golang.org/x/vuln/cmd/govulncheck@latest
/tmp/booking-review-wU8Ttp/bin/govulncheck -version
/tmp/booking-review-wU8Ttp/bin/govulncheck -show verbose ./...
docker create --name booking-review-wU8Ttp-extract booking-review-wu8ttp:local
docker cp booking-review-wU8Ttp-extract:/usr/local/bin/booking /tmp/booking-review-wU8Ttp/booking-docker
docker rm booking-review-wU8Ttp-extract
go version -m /tmp/booking-review-wU8Ttp/booking-docker
/tmp/booking-review-wU8Ttp/bin/govulncheck -mode=binary -show verbose /tmp/booking-review-wU8Ttp/booking-docker
GOTOOLCHAIN=go1.24.13 /tmp/booking-review-wU8Ttp/bin/govulncheck -show verbose ./...
```

Инструмент установлен вне проекта, go.mod/go.sum не изменены. Результаты и применимость приведены в разделе «Безопасность»; exit 3 — обнаруженные кандидаты, а не ошибка запуска инструмента.

## Не выполненные проверки и ограничения

- Не проводились тяжёлые нагрузочные атаки, длительный soak-тест, заполнение диска, исчерпание всех соединений, сетевые blackhole/failover-сценарии, аварийное выключение VM и проверка восстановления из backup. Они выходят за разумный объём небольшой задачи; один ограниченный сценарий долгой блокировки выполнен.
- Тесты не доказывают корректность для всех возможных расписаний конкурентных транзакций. Проверены конкретные параллельные сценарии и изучена семантика SQL/границ транзакций.
- Не проводилась эксплуатация известных advisory, проверка внешней доступности портов и полноценный penetration test. Все активные проверки ограничены собственным стендом; авторизация/TLS не добавлены и не требовались.
- Не сканировались системные пакеты Alpine/PostgreSQL сканером контейнерных CVE; govulncheck покрывает Go-код/модули и stdlib, а не весь образ.
- Буквальная команда `docker compose up --build` с исходными фиксированными портами 5432/8080 не запускалась: для изоляции использованы отдельное имя Compose-проекта и loopback/dynamic port override. Исходный Dockerfile собран, остальные настройки Compose и штатные healthchecks реально отработали. Воспроизводимость на других ОС/архитектурах не проверялась.

## Сильные стороны

- Транзакционные гарантии находятся в общей БД и подтверждены двумя отдельными процессами, а не зависят от состояния памяти сервера.
- Хорошо определена семантика идемпотентности: глобальный ключ, сравнение payload, повтор после отмены, отсутствие повторного списания мест.
- Атомарная условная отмена, уникальное ограничение ключа, CHECK/FK и сохранение отменённых броней защищают основные инварианты.
- README даёт рабочие команды, примеры и честно описывает ограничения миграций/хранения ключей; зависимости закреплены, Docker запускает приложение без root.
- Основные интеграционные тесты проверяют настоящую PostgreSQL и содержательные результаты конкурентных операций.

## Завершение стенда

После проверок выполнены:

```bash
docker compose -p booking-review-wu8ttp -f compose.yaml \
  -f /tmp/booking-review-wU8Ttp/compose.override.yaml down -v
docker rm -fv booking-review-wU8Ttp-pg
docker image rm booking-review-wu8ttp:local
git diff --exit-code
git status --short
```

Собственные тестовые контейнеры, их тома и тег образа удалены; временные скрипты/журналы оставлены для воспроизведения. Чужие ресурсы не затрагивались. Итоговый diff отслеживаемых файлов пуст, единственный новый файл — `codex_review.md`.

## Приложение: полный текст независимых проверок

Для воспроизведения создать собственный контейнер командой выше, собрать бинарник в указанную временную папку, сохранить следующие тексты как `check.py` и `timeout.py` рядом с бинарником. Если меняется имя контейнера/путь, соответственно заменить константы. Скрипты читают фактический порт через Docker. `check.py` создаёт уникальную схему; `timeout.py` использует только собственную БД контейнера и новые UUID. Запускать проверки последовательно, чтобы рестарт БД не мешал другим тестам.

### check.py

```python
import concurrent.futures as cf
import json, os, pathlib, socket, subprocess as sp, time, urllib.request as ur, urllib.error, uuid
ROOT = pathlib.Path(__file__).resolve().parent
PG = 'booking-review-wU8Ttp-pg'
def database_url():
    port=sp.check_output(['docker','port',PG,'5432'],text=True).strip().rsplit(':',1)[1]
    return 'postgres://booking:review-local-password@127.0.0.1:'+port+'/booking?sslmode=disable&search_path='+schema
schema='review_http_'+uuid.uuid4().hex

def sql(q):
    return sp.check_output(['docker','exec','-e','PGOPTIONS=-c search_path='+schema,PG,'psql','-U','booking','-d','booking','-Atc',q], text=True).strip()

def api(i, method, path, body=None, ctype='application/json'):
    data = body.encode() if isinstance(body,str) else json.dumps(body).encode() if body is not None else None
    req = ur.Request(urls[i]+path, data=data, method=method, headers={'Content-Type':ctype})
    try:
        response = ur.urlopen(req, timeout=12)
    except urllib.error.HTTPError as e:
        response = e
    with response:
        assert response.headers.get_content_type() == 'application/json'
        return response.status, json.load(response)

def parallel(n, f):
    with cf.ThreadPoolExecutor(max_workers=16) as pool:
        return list(pool.map(f, range(n)))

def start():
    result = []
    for i, url in enumerate(urls):
        env = dict(os.environ, DATABASE_URL=database_url(), HTTP_ADDR=url[7:])
        log = open(ROOT / ('app%d.log'%i), 'a')
        result.append(sp.Popen([str(ROOT/'booking')], env=env, stdout=log, stderr=log))
        log.close()
    for i in range(2):
        for _ in range(100):
            try:
                assert api(i,'GET','/healthz')[0] == 200
                break
            except (OSError, AssertionError):
                time.sleep(.1)
        else:
            raise RuntimeError('startup failed')
    return result

def stop(processes):
    for p in processes:
        p.terminate()
    for p in processes:
        assert p.wait(timeout=15) == 0

def event(cap):
    code, data = api(0,'POST','/events', {'title':'Review','capacity':cap})
    assert code == 201, (code,data)
    return data['id']

def book(i, eid, seats, key):
    return api(i,'POST','/bookings',dict(event_id=eid,seats=seats,idempotency_key=key))

urls=[]
for _ in range(2):
    with socket.socket() as s:
        s.bind(('127.0.0.1',0))
        urls.append('http://127.0.0.1:%d'%s.getsockname()[1])
sql('CREATE SCHEMA '+schema)
processes = start()  # Both processes migrate the same initially empty schema.
try:
    print('PASS simultaneous first startup', flush=True)
    eid = event(17)
    results = parallel(60,lambda n: book(n%2,eid,2,'oversell-%d'%n))
    assert sum(c==201 for c,_ in results) == 8
    assert sum(c==409 for c,_ in results) == 52
    assert api(1,'GET','/events/'+eid+'/availability')[1]['booked_seats'] == 16
    print('PASS two processes: 60 requests x 2 seats, capacity 17 => 8 success, 52 conflict',flush=True)
    idem_event = event(5)
    results = parallel(40,lambda n: book(n%2,idem_event,3,'shared-key'))
    assert sum(c==201 for c,_ in results) == 1
    assert sum(c==200 for c,_ in results) == 39
    bid = results[0][1]['id']
    assert {b['id'] for _,b in results} == {bid}
    assert book(1,idem_event,2,'shared-key')[0] == 409
    assert book(1,eid,3,'shared-key')[0] == 409
    results = parallel(40,lambda n: api(n%2,'DELETE','/bookings/'+bid))
    assert all(c==200 and b['status']=='cancelled' for c,b in results)
    assert len({b['cancelled_at'] for _,b in results}) == 1
    assert api(0,'GET','/events/'+idem_event+'/availability')[1]['available_seats'] == 5
    assert book(1,idem_event,3,'shared-key')[1]['status'] == 'cancelled'
    assert book(0,idem_event,5,'after-cancel')[0] == 201
    print('PASS 40 equal retries, key conflicts, 40 cancellations, cancelled replay and reuse of seats',flush=True)
    conflict_event=event(20)
    results=parallel(32,lambda n:book(n%2,conflict_event,1+n%2,'conflicting-key'))
    assert sum(c==201 for c,_ in results)==1
    assert sum(c==409 for c,_ in results)==16
    print('PASS concurrent same key with different payload',flush=True)
    mixed_event=event(10)
    def churn(n):
        code,b=book(n%2,mixed_event,1,'churn-%d'%n)
        assert code in (201,409)
        if code==201:
            assert api((n+1)%2,'DELETE','/bookings/'+b['id'])[0]==200
    parallel(80,churn)
    assert api(0,'GET','/events/'+mixed_event)[1]['booked_seats']==0
    invalid_events=[('',400),('null',400),('[]',400),('{}',400),('{',400),
        ('{"title":"X","capacity":0}',400),('{"title":"X","capacity":1000001}',400),
        ('{"title":"X","capacity":1.5}',400),('{"title":"X","capacity":9223372036854775808}',400),
        ('{"title":"X","capacity":1,"unknown":true}',400),
        ('{"title":"X","capacity":1} {}',400),
        (json.dumps({'title':'x'*201,'capacity':1}),400),
        (json.dumps({'title':'x\x00','capacity':1}),400),
        (json.dumps({'title':'x'*1048576,'capacity':1}),400)]
    for body,want in invalid_events:
        assert api(0,'POST','/events',body)[0]==want
    assert api(0,'POST','/events','{}','text/plain')[0]==415
    for seats,key in [(0,'x'),(-1,'x'),(1000001,'x'),(1,''),(1,'x'*201),(1,'я'*101),(1,'\x00')]:
        assert book(0,eid,seats,key)[0]==400
    assert api(0,'GET','/events/not-a-uuid')[0]==400
    assert book(0,'00000000-0000-0000-0000-000000000000',1,'missing')[0]==404
    assert api(0,'PATCH','/events/'+eid)[0]==405
    payload="'); DROP TABLE events; --"
    code, quoted=api(0,'POST','/events',dict(title=payload,capacity=1))
    assert code==201 and api(1,'GET','/events/'+quoted['id'])[1]['title']==payload
    assert book(0,quoted['id'],1,payload)[0]==201
    print('PASS mixed create/cancel, invalid JSON/limits/NUL/UUID/media type and SQL payload as literal',flush=True)
    assert sql("SELECT count(*) FROM events e WHERE e.booked_seats <> (SELECT coalesce(sum(seats),0) FROM bookings b WHERE b.event_id=e.id AND b.status='active')")=='0'
    saved=api(0,'GET','/bookings/'+bid)[1]
    stop(processes)
    processes=[]
    sp.run(['docker','restart',PG],check=True,stdout=sp.DEVNULL)
    for _ in range(100):
        if sp.run(['docker','exec',PG,'pg_isready','-U','booking','-d','booking'],stdout=sp.DEVNULL,stderr=sp.DEVNULL).returncode==0:
            break
        time.sleep(.1)
    processes=start()
    assert api(1,'GET','/bookings/'+bid)[1]==saved
    assert book(1,idem_event,3,'shared-key')[1]==saved
    assert api(0,'GET','/events/'+eid)[1]['booked_seats']==16
    print('PASS PostgreSQL restart, two app restarts, repeat migration with existing data and idempotency persistence',flush=True)
finally:
    stop(processes)
```

### timeout.py

```python
import concurrent.futures as cf
import http.client, json, os, pathlib, socket, subprocess as sp, time, urllib.request as ur
root=pathlib.Path(__file__).resolve().parent
pg='booking-review-wU8Ttp-pg'
port=sp.check_output(['docker','port',pg,'5432'],text=True).strip().rsplit(':',1)[1]
db='postgres://booking:review-local-password@127.0.0.1:'+port+'/booking?sslmode=disable'
def sql(q):
    return sp.check_output(['docker','exec',pg,'psql','-U','booking','-d','booking','-Atc',q],text=True).strip()
with socket.socket() as s:
    s.bind(('127.0.0.1',0))
    addr='127.0.0.1:%d'%s.getsockname()[1]
log=open(root/'timeout-app.log','w')
p=sp.Popen([str(root/'booking')],env=dict(os.environ,DATABASE_URL=db,HTTP_ADDR=addr),stdout=log,stderr=log)
lock=None
try:
    for _ in range(100):
        try:
            ur.urlopen('http://'+addr+'/healthz',timeout=1).close()
            break
        except OSError:
            time.sleep(.1)
    request=ur.Request('http://'+addr+'/events',data=b'{"title":"Timeout check","capacity":2}',headers={'Content-Type':'application/json'})
    eid=json.load(ur.urlopen(request))['id']
    lock=sp.Popen(['docker','exec','-i',pg,'psql','-U','booking','-d','booking','-qAt'],stdin=sp.PIPE,stdout=sp.PIPE,text=True)
    lock.stdin.write("BEGIN; SELECT id FROM events WHERE id='"+eid+"' FOR UPDATE; SELECT 'LOCKED';\n")
    lock.stdin.flush()
    while lock.stdout.readline().strip()!='LOCKED':
        assert lock.poll() is None
    key='timeout-'+eid
    def request_booking():
        c=http.client.HTTPConnection(addr,timeout=65)
        try:
            c.request('POST','/bookings',json.dumps(dict(event_id=eid,seats=1,idempotency_key=key)),{'Content-Type':'application/json'})
            r=c.getresponse()
            return r.status, r.read().decode()
        except Exception as e:
            return type(e).__name__,str(e)
        finally:
            c.close()
    with cf.ThreadPoolExecutor(max_workers=1) as executor:
        future=executor.submit(request_booking)
        time.sleep(32)
        waiting=sql("SELECT count(*) FROM pg_stat_activity WHERE wait_event_type='Lock' AND query LIKE '%SELECT capacity, booked_seats FROM events%'")
        assert waiting=='1' and not future.done(), (waiting,future.done())
        print('CONFIRMED: at 32 seconds request still waits for DB lock beyond WriteTimeout=30s',flush=True)
        p.terminate()
        try:
            p.wait(timeout=12)
            raise AssertionError('process unexpectedly exited within 12 seconds')
        except sp.TimeoutExpired:
            print('CONFIRMED: process still alive 12 seconds after SIGTERM despite Shutdown timeout=10s',flush=True)
        lock.stdin.write('ROLLBACK;\n\\q\n')
        lock.stdin.flush()
        lock.wait(timeout=5)
        lock=None
        print('HTTP result after lock release:',future.result(timeout=10),flush=True)
    print('Process exit after lock release:',p.wait(timeout=10),flush=True)
    print('Bookings committed after timeout and SIGTERM:',sql("SELECT count(*) FROM bookings WHERE idempotency_key='"+key+"'"),flush=True)
finally:
    if lock is not None:
        lock.stdin.write('ROLLBACK;\n\\q\n')
        lock.stdin.flush()
        lock.wait(timeout=5)
    if p.poll() is None:
        p.kill()
        p.wait()
    log.close()
```
