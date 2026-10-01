# fleeting-plugin-selectel

> A [GitLab Runner fleeting](https://docs.gitlab.com/runner/fleet_scaling/fleeting/) plugin that autoscales CI
> workers on [Selectel Cloud Servers](https://selectel.ru/services/cloud/servers/) (OpenStack): Linux over SSH and
> Windows over WinRM, placement fallback across availability zones/flavors, preemptible servers, floating IPs,
> private networks. The documentation below is in Russian.

[Fleeting plugin](https://docs.gitlab.com/runner/executors/docker_autoscaler/) для GitLab Runner (executor-ы
`docker-autoscaler` и `instance`), который создаёт и удаляет облачные серверы
[Selectel](https://selectel.ru/services/cloud/servers/) под нагрузку CI-джобов. Поддерживаются Linux (SSH) и Windows
(WinRM, статический пароль). По возможностям повторяет
[fleeting-plugin-yandex](https://github.com/AlexeySetevoi/yandex-fleeting-plugin).

## Как это работает

Плагин реализует интерфейс `provider.InstanceGroup` из `gitlab.com/gitlab-org/fleeting/fleeting` и запускается
GitLab Runner-ом как отдельный процесс. Облачные серверы Selectel — это OpenStack, плагин работает с его API (Nova,
Neutron, Glance) через [gophercloud v2](https://github.com/gophercloud/gophercloud); весь доступ к облаку спрятан за
узким интерфейсом в `internal/selectelapi`.

На каждый сервер плагин создаёт:

- **порт** в сети `network_id` — заранее, а не силами Nova: так группы безопасности задаются по ID, а публичный
  адрес привязывается до старта сервера;
- **floating IP** на этот порт (при `floating_ip = true`);
- **сервер** с загрузкой с сетевого диска, созданного из образа (`delete_on_termination` — диск удаляется вместе с
  сервером).

Свои серверы плагин узнаёт по metadata `fleeting-group=<name>` (плюс информационный
`managed-by=fleeting-plugin-selectel`) внутри проекта; порты и адреса — по description `fleeting-group=<name>`. Имя
сервера — `<name>-<8 hex>`, но принадлежность группе определяет metadata, поэтому отдельный проект под раннер не
обязателен — достаточно уникального `name`. Не меняйте эту metadata руками: сервер выпадет из группы и будет
биллиться, пока его не удалят вручную.

`Decrease` удаляет сервер, его floating IP и порт — после него в проекте ничего не остаётся. Если плагин упал
между созданием порта и сервера или сервер удалили руками, порт и адрес остаются без хозяина; раз в 5 минут `Update`
находит такие порты и адреса своей группы старше 10 минут и удаляет их.

## Сервисный пользователь

Плагину нужен [сервисный пользователь](https://docs.selectel.ru/control-panel-actions/users-and-roles/) с доступом к
проекту, в котором создаются серверы (роль уровня проекта, позволяющая создавать серверы, порты, floating IP, а при
`cores`/`memory_gb` — и приватные флейворы).

Учётные данные задаются в `plugin_config` либо переменными окружения из RC-файла, который панель отдаёт для
сервисного пользователя (IAM → сервисные пользователи → доступ → «Скачать RC-файл»). Переменные важнее конфига:

| переменная | поле `plugin_config` | что это |
|---|---|---|
| `OS_AUTH_URL` | `auth_url` | Keystone, по умолчанию `https://cloud.api.selcloud.ru/identity/v3` |
| `OS_USER_DOMAIN_NAME` | `account_id` | номер аккаунта Selectel |
| `OS_PROJECT_ID` | `project_id` | ID проекта |
| `OS_USERNAME` | `username` | имя сервисного пользователя |
| `OS_PASSWORD` | `password_file` | пароль (в конфиге — только путь к файлу с ним) |
| `OS_REGION_NAME` | `region` | пул: `ru-9`, `ru-7`, ... |

Переменные можно отдать раннеру через systemd:

```shell
sudo install -m 0600 -o root rc.sh /etc/gitlab-runner/selectel.env   # оставить только строки KEY=value, без export
sudo systemctl edit gitlab-runner
# [Service]
# EnvironmentFile=/etc/gitlab-runner/selectel.env
```

Либо файл с паролем (`0600`) и остальное в `plugin_config` — так сделано в примерах ниже. Токен Keystone живёт сутки,
плагин перевыпускает его сам.

## Сборка

```shell
go build -o fleeting-plugin-selectel ./cmd/fleeting-plugin-selectel
```

## Деплой

На каждый тег `X.Y.Z` GitHub Actions собирает бинари под `linux`/`darwin` × `amd64`/`arm64`, а для Linux ещё и
пакеты `deb`/`rpm`, и прикладывает всё к
[релизу](https://github.com/AlexeySetevoi/selectel-fleeting-plugin/releases). Пакеты `deb` вдобавок публикуются в
APT-репозиторий на GitHub Pages.

Пакет кладёт бинарь в `/usr/bin/fleeting-plugin-selectel`, поэтому в конфиге раннера достаточно имени:
`plugin = "fleeting-plugin-selectel"`.

### Debian/Ubuntu: APT-репозиторий

Один репозиторий на любой Debian и Ubuntu (бинарь статический), `amd64` и `arm64`. Обновление — обычным
`apt upgrade` вместе с системой.

```shell
sudo install -d -m 0755 /etc/apt/keyrings
curl -fsSL https://alexeysetevoi.github.io/selectel-fleeting-plugin/fleeting-plugin-selectel.gpg \
  | sudo tee /etc/apt/keyrings/fleeting-plugin-selectel.gpg >/dev/null
echo "deb [signed-by=/etc/apt/keyrings/fleeting-plugin-selectel.gpg] https://alexeysetevoi.github.io/selectel-fleeting-plugin stable main" \
  | sudo tee /etc/apt/sources.list.d/fleeting-plugin-selectel.list
sudo apt update
sudo apt install fleeting-plugin-selectel
```

Проверить, что скачан ключ проекта:

```shell
gpg --show-keys /etc/apt/keyrings/fleeting-plugin-selectel.gpg
```

Отпечаток должен совпадать с ключом `keys/fleeting-plugin-selectel.asc` в этом репозитории.

В репозитории только последняя версия; предыдущие — в релизах.

### Пакет или бинарь из релиза

```shell
# Debian/Ubuntu
curl -fsSLO "https://github.com/AlexeySetevoi/selectel-fleeting-plugin/releases/download/<версия>/fleeting-plugin-selectel_<версия>_amd64.deb"
sudo apt install ./fleeting-plugin-selectel_<версия>_amd64.deb

# RHEL/Rocky/Alma
sudo rpm -Uvh "https://github.com/AlexeySetevoi/selectel-fleeting-plugin/releases/download/<версия>/fleeting-plugin-selectel-<версия>-1.x86_64.rpm"
```

Для arm64 — `_arm64.deb` и `.aarch64.rpm`.

Либо просто бинарь:

```shell
curl -fsSL -o fleeting-plugin-selectel \
  "https://github.com/AlexeySetevoi/selectel-fleeting-plugin/releases/download/<версия>/fleeting-plugin-selectel-linux-amd64"
chmod +x fleeting-plugin-selectel
```

### Проверка подлинности

- **GPG-ключ проекта** (`keys/fleeting-plugin-selectel.asc`, он же лежит в каждом релизе и на Pages) — им подписан
  APT-репозиторий (`InRelease`, `apt` проверяет его сам) и `SHA256SUMS` в релизе (`SHA256SUMS.asc`).

Остальное даёт сам GitHub (подписи Sigstore, привязанные к репозиторию, workflow, коммиту и тегу):

- **аттестация происхождения** (build provenance) — на каждый файл релиза: бинари, `deb`, `rpm`, SBOM, `SHA256SUMS`;
- **аттестация SBOM** — к каждому бинарю и пакету привязан SBOM (SPDX, снят с бинаря через syft); сами
  `*.spdx.json` тоже лежат в релизе;
- **неизменяемые релизы** — тег и файлы после публикации подменить нельзя, GitHub сам выпускает аттестацию релиза;
- `SHA256SUMS` — для проверки целостности без `gh`;
- сборка воспроизводима (`-trimpath`, время из коммита, фиксированный `buildhost` в rpm): пересборка тега на
  свежем клоне даёт те же контрольные суммы бинарей и пакетов, что в релизе. SBOM-файлы не совпадут — в них время
  генерации.

Проверить воспроизводимость самому:

```shell
git clone --branch <версия> https://github.com/AlexeySetevoi/selectel-fleeting-plugin.git && cd selectel-fleeting-plugin
GITHUB_TOKEN=$(gh auth token) goreleaser release --clean --skip=publish   # токен нужен только для текста changelog
grep -v spdx.json dist/SHA256SUMS | sort   # сравнить с SHA256SUMS из релиза
```

```shell
R=AlexeySetevoi/selectel-fleeting-plugin
F=fleeting-plugin-selectel_<версия>_amd64.deb

gh attestation verify $F -R $R                                                    # происхождение
gh attestation verify $F -R $R --predicate-type https://spdx.dev/Document/v2.3    # SBOM
gh release verify <версия> -R $R                                                  # релиз не подменён
gh release verify-asset <версия> $F -R $R                                         # файл именно из этого релиза

gpg --import fleeting-plugin-selectel.asc   # без gh: подпись SHA256SUMS ключом проекта
gpg --verify SHA256SUMS.asc SHA256SUMS
sha256sum -c SHA256SUMS --ignore-missing
```

Нужен свежий `gh` с [cli.github.com](https://cli.github.com/): в 2.46 из репозитория Ubuntu команд `attestation` и
`release verify` ещё нет.

Сами пакеты не подписаны: `rpm -K` и `dnf` подпись не видят; для `apt` подписан репозиторий, для файлов из
релиза — `SHA256SUMS`.

1. Поставить пакет либо положить бинарь на хост, где крутится `gitlab-runner` (например,
   `/etc/gitlab-runner/plugins/fleeting-plugin-selectel`, `root:root`, `0755`).
2. Указать в `plugin` секции `[runners.autoscaler]` имя (для пакета) или полный путь (для бинаря).
3. `systemctl restart gitlab-runner` — новый бинарь плагина подхватывается только при рестарте раннера. Правки
   самого `config.toml` раннер перечитывает сам.

## Конфигурация (`plugin_config`)

| поле | обязательное | описание |
|---|---|---|
| `name` | да | имя группы: префикс имён серверов и значение metadata. `^[a-z][-a-z0-9]*$`, до 54 символов |
| `account_id` | да* | номер аккаунта (`OS_USER_DOMAIN_NAME`) |
| `project_id` | да* | проект, в котором создаются серверы (`OS_PROJECT_ID`) |
| `username` | да* | сервисный пользователь (`OS_USERNAME`) |
| `password_file` | да* | файл с паролем сервисного пользователя (`OS_PASSWORD`) |
| `region` | да* | пул, например `ru-9` (`OS_REGION_NAME`) |
| `auth_url` | нет | Keystone, по умолчанию `https://cloud.api.selcloud.ru/identity/v3` (`OS_AUTH_URL`) |
| `availability_zone` | да** | зона пула, например `ru-9a` |
| `flavor` | да*** | флейвор — имя (`SL1.2-4096`) или ID |
| `placements` | да** | список `{availability_zone, flavor}` вместо двух полей выше, см. ниже |
| `cores` / `memory_gb` | да*** | произвольная конфигурация вместо `flavor`; `memory_gb` можно дробным (`0.5`) |
| `image_id` | да**** | ID образа |
| `image_name` | да**** | имя образа (`Ubuntu 24.04 LTS 64-bit`); самый свежий образ с этим именем ищется при каждом создании |
| `volume_type` | нет | тип загрузочного диска без зоны: `universal` (по умолчанию), `fast`, `basic`, ...; зона дописывается сама |
| `disk_size_gb` | да | размер загрузочного диска, не меньше минимального для образа |
| `network_id` | да | сеть, в которой создаётся порт сервера |
| `subnet_id` | нет | подсеть этой сети, если их несколько |
| `security_group_ids` | нет | группы безопасности порта (ID); без них — группа проекта по умолчанию |
| `floating_ip` | нет | выдать публичный адрес (floating IP); удаляется вместе с сервером |
| `floating_network_id` | нет | внешняя сеть для floating IP; по умолчанию — единственная внешняя сеть проекта |
| `preemptible` | нет | прерываемые серверы, см. ниже |
| `tags` | нет | теги сервера; `preemptible` зарезервирован |
| `metadata` | нет | metadata сервера; `fleeting-group` и `managed-by` зарезервированы |
| `user_data` / `user_data_file` | нет | cloud-init (Linux) или cloudbase-init (Windows); файл читается один раз при старте |

\* в конфиге или переменной окружения. \*\* либо `availability_zone`, либо `placements`. \*\*\* либо `flavor` (в
том числе в каждом элементе `placements`), либо `cores` + `memory_gb`. \*\*\*\* ровно одно из двух.

Конфиг разбирается строго: незнакомый ключ в `plugin_config` (опечатка вроде `preemtible`) или значение не того типа —
ошибка при старте раннера, а не молча проигнорированная настройка. Имена флейворов переводятся в ID при старте,
несуществующий флейвор — тоже ошибка старта.

**Флейворы.** Список фиксированных конфигураций — `openstack flavor list` или
[документация](https://docs.selectel.ru/cloud-servers/create/configurations/): линейки Standard (`SL1`), Shared
(доля vCPU — аналог `core_fraction`), HighFreq, GPU и другие. Для загрузки с сетевого диска нужен флейвор с
`disk = 0`. `cores` + `memory_gb` — произвольная конфигурация: плагин один раз заводит приватный флейвор
`fleeting-<cores>-<ram_mb>-<hex>` с `disk = 0` и дальше переиспользует его (Nova не даёт повторить имя даже
удалённого флейвора, отсюда случайный хвост). Публичные флейворы с такими же размерами не подбираются: это может
оказаться другая линейка по другой цене.

Проверено вживую (ru-9): приватный флейвор создаёт обычный участник проекта, Selectel сам проставляет ему
`line=standard` и `fl_size=flex` — это произвольная конфигурация линейки Standard. Пределы — не больше 68 vCPU и
716800 МБ (700 ГБ) RAM, больше API отклоняет при старте плагина (`Flavor specification failed validation`).

## Фолбэк по зонам и флейворам (`placements`)

```toml
[runners.autoscaler.plugin_config]
  # ...
  placements = [
    { availability_zone = "ru-9a", flavor = "SL1.4-8192" },
    { availability_zone = "ru-9b", flavor = "SL1.4-8192" },
    { availability_zone = "ru-9a", flavor = "HF1.4-8192" },
  ]
```

Сеть в OpenStack региональная, поэтому в размещение входят только зона и флейвор; тип диска подставляется под зону
сам (`universal.ru-9a`, `universal.ru-9b`). Весь `placements` — внутри одного пула (`region`).

Для каждого нового сервера варианты пробуются по порядку. К следующему плагин переходит только если облако
ответило ошибкой квоты («Quota exceeded», `OverQuota`, `VolumeLimitExceeded`) — любая другая ошибка (права, неверный
образ, несуществующая сеть) от смены зоны не лечится и возвращается сразу.

Отказ бывает двух видов, и обрабатываются они по-разному:

- **сразу, в ответе на запрос** (квота, права, конфигурация) — следующий вариант пробуется тут же, в том же запросе
  раннера;
- **позже, когда планировщик Nova не нашёл места** — к этому моменту запрос уже принят, а плагин ответил раннеру.
  За сервером следит фоновая горутина: сервер уходит в `ERROR` с «No valid host was found», ошибка пишется в лог
  раннера (`instance creation failed after the request was accepted`), размещение на 10 минут уходит в конец
  очереди. Сервер в `ERROR` плагин отдаёт раннеру как `timeout`, раннер удаляет его и запрашивает замену — она идёт
  уже в следующее размещение. Если ресурсы кончились везде, варианты всё равно пробуются по порядку из конфига.

Плагин не ждёт создания сервера: цикл раннера однопоточный, и пока `Increase` не вернулся, не обновляются состояния и
не удаляются простаивающие машины. Запросы уходят параллельно (до 5 одновременно), принятый сервер сразу виден
раннеру как `creating`.

Вживую `Increase` на один сервер возвращается за 4–5 секунд (образ, порт, floating IP и сервер — последовательные
запросы), на два — за те же ~5,5 секунды; до `ACTIVE` сервер доходит за 20–25 секунд, SSH отвечает сразу.

Что не проверено вживую: в новом проекте квоты Nova не ограничены (`openstack limits show --absolute` — везде
`-1`), лимиты Selectel живут в его собственном слое, и сервер на 32 vCPU / 64 ГБ создался без вопросов. Поэтому
ни ошибку квоты, ни «No valid host» по заказу получить не удалось (не создавая дорогих серверов). Если облако
ответит на них иначе, код будет виден в логе раннера, а условие перехода задаётся в одном месте — `mapError` и
`faultError` в `internal/selectelapi/openstack.go`.

## Пример: Linux-раннер по SSH

```toml
[[runners]]
  name = "selectel-docker-autoscaler"
  executor = "docker-autoscaler"

  [runners.docker]
    image = "alpine:latest"

  [runners.autoscaler]
    plugin = "fleeting-plugin-selectel"

    capacity_per_instance = 1
    max_use_count = 10
    max_instances = 10
    instance_ready_command = "cloud-init status --wait || test $? -eq 2"

    [runners.autoscaler.plugin_config]
      name              = "ci-linux"
      account_id        = "123456"
      project_id        = "0123456789abcdef0123456789abcdef"
      username          = "gitlab-fleeting"
      password_file     = "/etc/gitlab-runner/selectel-password"
      region            = "ru-9"
      availability_zone = "ru-9a"
      flavor            = "SL1.4-8192"
      image_name        = "Ubuntu 24.04 LTS 64-bit"
      disk_size_gb      = 50
      network_id        = "<ID приватной сети>"
      floating_ip       = true
      user_data_file    = "/etc/gitlab-runner/docker-cloud-init.yaml"

    [runners.autoscaler.connector_config]
      username = "root"
      protocol = "ssh"
      use_external_addr = true

    [[runners.autoscaler.policy]]
      idle_count = 1
      idle_time  = "20m0s"
```

SSH-ключ: плагин при старте генерирует пару ed25519 и импортирует публичную часть в Nova как keypair
`fleeting-<name>-<hex>`; серверы получают её через `key_name`, образы Selectel кладут ключ пользователю `root`,
поэтому `username` по умолчанию `root`. Keypair своя на каждый запуск плагина и удаляется при его остановке (уже
созданным серверам она не нужна — ключ в них уже лежит); после аварийного завершения лишняя keypair остаётся, она
ничего не стоит. Keypair в OpenStack принадлежит пользователю — видна она только самому сервисному пользователю.
Свой ключ — `use_static_credentials = true` + `key_path` в `connector_config`: плагин возьмёт из него публичную
часть.

`use_external_addr = true` нужен, когда раннер-менеджер снаружи облака и ходит на серверы по публичному адресу (тогда
обязателен и `floating_ip = true`). Если менеджер в той же сети — уберите оба: подключение пойдёт по внутреннему
адресу, публичные адреса не тратятся. Для floating IP у приватной подсети должен быть облачный роутер с выходом во
внешнюю сеть.

Проверено вживую smoke-тестом (ru-9, `Ubuntu 24.04 LTS 64-bit`): загрузка с сетевого диска из образа, вход по SSH
под `root` через `key_name`, floating IP, два сервера одним `Increase`, произвольная конфигурация через
`cores`/`memory_gb`, тег `preemptible`; после `Decrease` в проекте не остаётся ни серверов, ни дисков, ни портов, ни
адресов, ни keypair. Порт, брошенный удалённым вручную сервером, убирается первым же `Update` после 10 минут. Полный
цикл под настоящим `gitlab-runner` на Selectel ещё не гонялся.

`instance_ready_command` обязателен, если Docker ставится через cloud-init: сервер переходит в `ACTIVE` раньше, чем
cloud-init закончит работу, и без проверки первая джоба упадёт на отсутствующем Docker.

### Docker через cloud-init

```yaml
#cloud-config
package_update: true
packages:
  - docker.io
runcmd:
  - systemctl enable --now docker
```

Быстрее и надёжнее — собрать свой образ с уже установленным Docker (Packer) и указать его через `image_id` или
`image_name`.

## Приватная сеть: воркеры без публичных адресов

Если раннер-менеджер живёт в той же приватной сети (или ходит в неё по VPN), публичные адреса воркерам не нужны:
уберите `floating_ip` из `plugin_config` и `use_external_addr` из `connector_config` — подключение пойдёт по
внутреннему адресу. Чтобы воркеры при этом ходили наружу (`apt`, `docker pull`, клонирование репозиториев), у подсети
должен быть облачный роутер с выходом в интернет.

Проверено вживую: smoke-тест с сервера внутри сети, без floating IP, вход на воркер по внутреннему адресу,
наружу воркер ходит через SNAT роутера.

Сеть можно дать и публичную (подсеть с прямыми публичными адресами): тогда `network_id` — её ID, `floating_ip` не
нужен, внешним адресом сервера считается его fixed-адрес из публичного диапазона.

## Прерываемые серверы (`preemptible`)

[Прерываемые серверы](https://docs.selectel.ru/cloud-servers/about/preemptible-servers/) заметно дешевле, но облако
может остановить их в любой момент и обязательно останавливает через 24 часа. Плагин ставит серверу тег
`preemptible` (Nova API 2.72). Вытесненный сервер получает статус `EXPIRED`; плагин отдаёт для него состояние
`timeout`, и taskscaler удаляет его и при необходимости создаёт новый. Джоба, выполнявшаяся на прерванной машине,
упадёт — для таких раннеров имеет смысл `retry` в `.gitlab-ci.yml` и небольшой `max_use_count`. Прерываемые серверы
доступны не во всех пулах — см. документацию Selectel.

По той же причине не выключайте серверы группы руками: плагин машины только создаёт и удаляет, сервер в `SHUTOFF`
будет снесён.

## Пример: Windows-раннер по WinRM

Плагин не получает пароль администратора, поэтому для WinRM обязателен `use_static_credentials = true` — без него
плагин не стартует. Учётные данные должны быть в самом образе (собственный образ с запечённым паролем
`Administrator`, включённым WinRM и HTTPS-слушателем) или задаваться через `user_data`, если в образе есть
cloudbase-init.

```toml
[[runners]]
  name = "selectel-windows"
  executor = "instance"     # джобы идут прямо в шелле сервера, Docker на Windows не нужен
  shell = "powershell"

  [runners.autoscaler]
    plugin = "fleeting-plugin-selectel"
    capacity_per_instance = 1
    max_use_count = 20
    max_instances = 2

    [runners.autoscaler.plugin_config]
      name               = "ci-windows"
      account_id         = "123456"
      project_id         = "0123456789abcdef0123456789abcdef"
      username           = "gitlab-fleeting"
      password_file      = "/etc/gitlab-runner/selectel-password"
      region             = "ru-9"
      availability_zone  = "ru-9a"
      cores              = 8
      memory_gb          = 16
      image_id           = "<ID своего Windows-образа>"
      volume_type        = "fast"
      disk_size_gb       = 120
      network_id         = "<ID приватной сети>"
      floating_ip        = true
      security_group_ids = ["<ID группы: 5986 только с адреса раннер-менеджера>"]

    [runners.autoscaler.connector_config]
      username = "Administrator"
      password = "<пароль>"
      protocol = "winrm+https"
      use_static_credentials = true
      timeout = "45m"
```

- **`winrm+https`, а не `winrm`.** Коннектор раннера ходит по NTLM без шифрования сообщений, поэтому по HTTP он
  работает только с `AllowUnencrypted=true` на стороне Windows. По HTTPS сертификат не проверяется — подойдёт
  самоподписанный.
- Пароль лежит в `config.toml` открытым текстом: права `0600`, порт 5986 — только с адреса раннер-менеджера.
- **Свой образ — только в формате raw.** Образы Selectel лежат в Ceph: из raw-образа загрузочный диск получается
  клоном за секунды, а qcow2 скачивается и конвертируется в каждый новый диск заново. Вживую (ru-9, образ Windows
  Server 2022 с UE 5.8, qcow2 28 ГБ, диск 100 ГБ `fast`) один диск из qcow2 собирался 27–38 минут — сервер всё это
  время висит в `BUILD`, и раннер не дождётся. Тот же образ в raw: от запроса до входа по WinRM около 5 минут.
  Заливать 100 ГБ raw из дома не обязательно — конвертацию можно сделать на стороне облака, один раз:

  ```shell
  openstack image create --disk-format qcow2 --container-format bare --file image.qcow2 win-qcow2
  openstack volume create --image win-qcow2 --size 100 --type fast.ru-9a --availability-zone ru-9a win-tmp   # ~30 мин
  openstack image create --volume win-tmp --disk-format raw --container-format bare win-raw                  # ~70 мин на 100 ГБ
  openstack image set win-raw --property os_type=windows --property hw_qemu_guest_agent=yes
  openstack volume delete win-tmp && openstack image delete win-qcow2
  ```

  Образ собран на `virtio-scsi` с полным набором virtio-драйверов — на дисках Selectel загрузился без доработок.
- Если сервер удалить, пока его диск ещё создаётся, диск не остаётся: Nova удаляет его сама, как только он
  создастся (проверено вживую).
- Лицензирование Windows на облачных серверах Selectel устроено по-своему (публичные Windows-образы
  лицензируются отдельно) — плагин с ними не проверялся, лицензия своего образа — на вашей стороне.

Проверено вживую: smoke-тест с собственным образом (Windows Server 2022 + UE 5.8, пароль запечён при сборке,
`winrm+https`) — вход по WinRM проходит, после `Decrease` в проекте ничего не остаётся.

## Расписание: тёплая машина в рабочие часы

Сколько машин держать в простое, решает не плагин, а сам GitLab Runner через `[[runners.autoscaler.policy]]` с
полями `periods` (unix-cron) и `timezone`:

```toml
# базовая политика: ночью и в выходные скейлимся до нуля
[[runners.autoscaler.policy]]
  idle_count = 0
  idle_time  = "20m0s"

# будни 9:00–17:59 МСК: одна машина всегда тёплая
[[runners.autoscaler.policy]]
  periods    = ["* 9-17 * * mon-fri"]
  timezone   = "Europe/Moscow"
  idle_count = 1
  idle_time  = "10h0m0s"
```

Применяется последний совпавший блок. **Грабли:** taskscaler отсчитывает `idle_time` от создания сервера, а не от
его готовности. Если образ разворачивается долго (Windows), а `idle_time` сопоставим с этим временем, тёплая машина
будет сноситься и пересоздаваться по кругу. В «тёплом» блоке ставьте `idle_time` больше самого окна — вечером машину
удалит базовая политика. С `preemptible` тёплая машина всё равно проживёт не дольше 24 часов.

## CI/CD

- `.github/workflows/ci.yml` — на каждый пуш в `main` и pull request: `go mod tidy -diff`, `go mod verify`, `vet`,
  тесты с `-race` (покрытие — в summary джобы), `golangci-lint` (`.golangci.yml`) и пробная сборка релиза
  [GoReleaser](https://goreleaser.com/)-ом без публикации — конфиг релиза проверяется постоянно, а не в момент выпуска.
  Из пробных `deb` собирается APT-репозиторий одноразовым ключом, и пакет ставится через `apt` в чистых
  `ubuntu:26.04`, `ubuntu:24.04`, `debian:13` на `amd64` и `arm64` (`scripts/test-apt-repo.sh`).
- `.github/workflows/release.yml` — на тег `X.Y.Z`: сначала весь `ci`, затем GoReleaser (`.goreleaser.yaml`) собирает
  бинари `linux/darwin` × `amd64/arm64`, `deb`/`rpm`, SBOM и `SHA256SUMS` и создаёт релиз-черновик; `SHA256SUMS`
  подписывается ключом проекта, выпускаются аттестации, собирается APT-репозиторий (`scripts/build-apt-repo.sh`) и
  выкладывается на Pages, и последним шагом релиз публикуется. Ключ — в секретах `APT_SIGNING_KEY` и
  `APT_SIGNING_PASSPHRASE`.
- `.github/workflows/govulncheck.yml` — `govulncheck` на пуш и раз в неделю. Отдельно от `ci`, чтобы уязвимость в
  зависимости, для которой ещё нет исправления, была видна, но не блокировала релизы.
- `.github/dependabot.yml` — еженедельные обновления Go-модулей и экшенов. Экшены закреплены по SHA коммита.

Выпуск версии: `git tag 1.0.0 && git push origin 1.0.0`. Создавать релиз руками в интерфейсе не нужно и нельзя:
релизы неизменяемые (immutable releases), к опубликованному релизу файлы не добавить.

## Suspend/Resume

Не поддерживаются: `Decrease` всегда удаляет сервер.

## Разработка

```shell
go build ./...
go vet ./...
go test ./... -count=1 -race
golangci-lint run ./...
goreleaser release --snapshot --clean --skip=publish   # локальная пробная сборка релиза, нужен syft
```

Тесты работают без доступа к облаку:

- `provider.InstanceGroup` — на фейковой реализации `selectelapi.Cloud`: отбор своих серверов по metadata, сборка
  запроса (metadata, теги, флейвор, тип диска под зону, floating IP, keypair, значения по умолчанию), учётные данные
  из переменных окружения и файла, фолбэк по `placements` (и сразу, и после ухода сервера в `ERROR`, с возвратом
  размещения по таймауту), `Increase` не блокируется на создании, частичный успех `Increase`, `Decrease` с уже
  удалённым сервером, уборка сирот, ошибки `Init`;
- `internal/selectelapi` — через настоящий gophercloud к локальному HTTP-серверу с фейковыми Keystone (каталог
  сервисов по пулам), Nova, Neutron и Glance: аутентификация и выбор endpoint-ов своего пула, микроверсия, пагинация,
  тела запросов на порт, floating IP и сервер (BDM, `key_name`, `user_data`), уборка порта и адреса, если сервер не
  принят, ожидание `ACTIVE` и разбор fault, удаление, поиск сирот, флейворы, образы, внешняя сеть, keypair,
  маппинг ошибок;
- разбор конфига тем же путём, что в бою (TOML → JSON → структура), включая все примеры `toml` из этого README:
  пример, который плагин не примет, уронит тест;
- валидация конфига и маппинг статусов, включая сверку со списком статусов Nova — статус, добавленный в список, но
  не в маппинг, уронит тест.

Покрытие: `go test ./... -coverprofile=coverage.out && go tool cover -func=coverage.out`.

### Смоук-тест на реальном проекте

Фейковый сервер не знает, как ведёт себя настоящее облако (коды ошибок при нехватке ресурсов, время до `ACTIVE`,
работа keypair на конкретном образе) — это проверяет `cmd/smoketest`: Init → Increase(`-count`, по умолчанию 1) →
ожидание `ACTIVE` → ConnectInfo → реальный вход по SSH/WinRM тем же пакетом `fleeting/connector`, что использует
GitLab Runner → Decrease → Shutdown. Создаёт настоящий сервер (и удаляет его, если не задан `-keep`).

```shell
set -a; . ./rc.sh; set +a     # RC-файл сервисного пользователя
go run ./cmd/smoketest \
  -availability-zone=ru-9a -flavor=SL1.1-2048 -network-id=... \
  -image-name="Ubuntu 24.04 LTS 64-bit" -floating-ip
```

Реальный код ошибки квоты удобно смотреть одиночным прогоном с заведомо невыполнимым запросом — сервер при этом не
создаётся:

```shell
... -cores=200 -memory-gb=1024
```

После прогона в проекте не должно остаться ничего от группы:

```shell
openstack server list --name '^smoketest-'
openstack port list -f value -c Name | grep '^smoketest-'
openstack floating ip list        # в отдельном тестовом проекте должно быть пусто
openstack volume list
openstack keypair list | grep fleeting-smoketest
```

Остальные флаги — `go run ./cmd/smoketest -h`.

## Лицензия

[MIT](LICENSE).
