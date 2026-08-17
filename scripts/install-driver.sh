#!/usr/bin/env bash
# Ставит драйвер Playwright в обход CDN Microsoft.
#
# playwright-go тянет драйвер с playwright.azureedge.net, который выключен, а
# зеркало cdn.playwright.dev отдаёт GatewayExceptionResponse. При этом ровно
# тот же пакет лежит в npm-реестре и оттуда качается нормально — этим и
# пользуемся.
#
# Браузер скрипт не качает: используйте системный Chrome через
# BROWSER_CHANNEL=chrome в .env.

set -euo pipefail

# Должна совпадать с playwrightCliVersion в playwright-go.
VERSION="${PLAYWRIGHT_VERSION:-1.60.0}"

case "$(uname -s)" in
	Darwin) CACHE="$HOME/Library/Caches" ;;
	*)      CACHE="${XDG_CACHE_HOME:-$HOME/.cache}" ;;
esac

DRIVER_DIR="${PLAYWRIGHT_DRIVER_PATH:-$CACHE/ms-playwright-go/$VERSION}"
TARBALL="https://registry.npmjs.org/playwright-core/-/playwright-core-$VERSION.tgz"

if ! command -v node >/dev/null 2>&1; then
	echo "Нужен Node.js — playwright-go запускает драйвер через него." >&2
	exit 1
fi

echo "Версия драйвера: $VERSION"
echo "Каталог:         $DRIVER_DIR"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

echo "Качаю $TARBALL"
curl -fsSL --max-time 300 -o "$TMP/core.tgz" "$TARBALL"
tar -xzf "$TMP/core.tgz" -C "$TMP"

if [ ! -f "$TMP/package/cli.js" ]; then
	echo "В пакете нет cli.js — playwright-go такой драйвер не примет." >&2
	exit 1
fi

mkdir -p "$DRIVER_DIR"
rm -rf "$DRIVER_DIR/package"
cp -R "$TMP/package" "$DRIVER_DIR/package"
ln -sf "$(command -v node)" "$DRIVER_DIR/node"

# Ровно та проверка, которую делает playwright-go перед запуском.
echo -n "Проверка: "
"$DRIVER_DIR/node" "$DRIVER_DIR/package/cli.js" --version

echo "Готово. Не забудьте BROWSER_CHANNEL=chrome в .env"
