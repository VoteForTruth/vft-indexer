# Деплой на сервер `nous`

По правилам Nous: без Docker, свой пользователь, systemd, `/opt` + `/etc` + `/var/lib`, Caddy.

| Что | Где |
|---|---|
| бинарник | `/opt/vft-indexer/vft-indexer` (меняет `../deploy.sh`) |
| настройки | `/etc/vft-indexer/env` (образец — `env.example`) |
| база SQLite | `/var/lib/vft-indexer/vft-indexer.db` |
| юнит | `/etc/systemd/system/vft-indexer.service` (копия — `vft-indexer.service`) |
| API | `127.0.0.1:8090`, снаружи — `https://votefortruth.org/api/*` (Caddy срезает `/api`) |

## Первая установка (один раз)

```bash
sudo useradd --system --no-create-home --shell /usr/sbin/nologin vft-indexer
sudo install -d -o root -g root -m 755 /opt/vft-indexer /etc/vft-indexer
sudo install -d -o vft-indexer -g vft-indexer -m 750 /var/lib/vft-indexer
sudo install -m 644 env.example /etc/vft-indexer/env            # поправить при необходимости
sudo install -m 644 vft-indexer.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable vft-indexer
../deploy.sh                                                     # бинарник и запуск
```

## Модерация

```bash
sudo -u vft-indexer /opt/vft-indexer/vft-indexer -db /var/lib/vft-indexer/vft-indexer.db hide <statement> "<причина>"
sudo -u vft-indexer /opt/vft-indexer/vft-indexer -db /var/lib/vft-indexer/vft-indexer.db unhide <statement>
```

## Смена сети или мастера

Поправить `VFT_NETWORK` / `VFT_MASTER` в `/etc/vft-indexer/env`, удалить базу (данные
другого мастера не нужны, всё восстановится сканированием) и перезапустить:
`sudo systemctl stop vft-indexer && sudo rm -f /var/lib/vft-indexer/vft-indexer.db* && sudo systemctl start vft-indexer`.
