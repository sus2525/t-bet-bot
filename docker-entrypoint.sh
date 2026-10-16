#!/bin/sh
set -eu

data_dir=${DATA_FILE:-/data/bot.db}
data_dir=${data_dir%/*}
if [ -z "$data_dir" ]; then
  data_dir=/
fi

mkdir -p "$data_dir"
chown -R bot:bot "$data_dir"
chmod -R u+rwX "$data_dir"

exec su-exec bot:bot /usr/local/bin/football-bot "$@"
