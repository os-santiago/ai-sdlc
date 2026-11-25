# Scheduled PostgreSQL backup and operator-started restore
# (docs/operations/backup-and-restore.md). POSIX sh: it runs under the
# PostgreSQL image's busybox shell.
#
# Inputs (environment):
#   DATABASE_URL      the bootstrap owner's postgresql:// URL, from a Secret
#   BACKUP_DIR        the backup volume
#   BACKUP_RETENTION  how many complete backups to keep (at least 1)
#   RESTORE_FILE      set only by an operator's restore Job: the backup file
#                     name to restore instead of taking a backup
#
# The password never reaches a command line or the log: it is split from the
# URL into PGPASSWORD, and the PostgreSQL tools receive the URL without it.
set -eu
umask 077

fail() {
  echo "backup: $*" >&2
  exit 1
}

dir="${BACKUP_DIR:?BACKUP_DIR is required}"
retention="${BACKUP_RETENTION:?BACKUP_RETENTION is required}"
case "$retention" in
  '' | *[!0-9]* | 0*) fail "BACKUP_RETENTION must be a positive integer" ;;
esac
[ -d "$dir" ] || fail "backup directory is missing"

uri="${DATABASE_URL:?DATABASE_URL is required}"
case "$uri" in
  postgresql://* | postgres://*) ;;
  *) fail "DATABASE_URL must be a postgresql:// URL" ;;
esac
scheme="${uri%%://*}"
rest="${uri#*://}"
authority="${rest%%/*}"
path="${rest#"$authority"}"
case "$authority" in
  *@*)
    userinfo="${authority%@*}"
    hostport="${authority##*@}"
    ;;
  *)
    userinfo=""
    hostport="$authority"
    ;;
esac
case "$userinfo" in
  *:*)
    user="${userinfo%%:*}"
    password="${userinfo#*:}"
    ;;
  *)
    user="$userinfo"
    password=""
    ;;
esac
case "$password" in
  *%*) fail "a percent-encoded password in DATABASE_URL is not supported" ;;
esac
[ -n "$hostport" ] || fail "DATABASE_URL has no host"
unset DATABASE_URL uri rest authority userinfo
if [ -n "$password" ]; then
  PGPASSWORD="$password"
  export PGPASSWORD
fi
unset password
if [ -n "$user" ]; then
  conninfo="$scheme://$user@$hostport$path"
else
  conninfo="$scheme://$hostport$path"
fi

if [ -n "${RESTORE_FILE:-}" ]; then
  # Restore: replace the database's objects and rows with the archive's, in
  # one transaction. Refuse while any runtime component is still connected.
  echo "$RESTORE_FILE" | grep -Eq '^ai-sdlc-[0-9]{8}T[0-9]{6}Z\.dump$' || fail "RESTORE_FILE must name a backup file"
  [ -f "$dir/$RESTORE_FILE" ] || fail "$RESTORE_FILE is not on the backup volume"
  pg_restore --list "$dir/$RESTORE_FILE" > /dev/null || fail "$RESTORE_FILE is unreadable"
  sessions="$(psql --no-password --no-psqlrc --tuples-only --no-align --dbname="$conninfo" \
    --command="SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND usename LIKE 'aisdlc\_%' AND pid <> pg_backend_pid()")" ||
    fail "cannot connect to the database"
  [ "$sessions" = 0 ] || fail "$sessions runtime sessions are connected; scale the components to zero first"
  echo "backup: restoring $RESTORE_FILE"
  pg_restore --clean --if-exists --no-owner --single-transaction --exit-on-error --no-password \
    --dbname="$conninfo" "$dir/$RESTORE_FILE" || fail "pg_restore failed; the transaction was rolled back"
  echo "backup: restored $RESTORE_FILE"
  exit 0
fi

[ -w "$dir" ] || fail "backup directory is not writable"
# Remove partial files a killed run left behind. They never count as backups.
for partial in "$dir"/.ai-sdlc-*.dump.partial; do
  [ -e "$partial" ] || continue
  rm -f -- "$partial"
done

stamp="$(date -u +%Y%m%dT%H%M%SZ)"
name="ai-sdlc-$stamp.dump"
target="$dir/$name"
temporary="$dir/.$name.partial"
[ ! -e "$target" ] || fail "$name already exists"

echo "backup: dumping to $name"
if ! pg_dump --format=custom --no-password --file="$temporary" --dbname="$conninfo"; then
  rm -f -- "$temporary"
  fail "pg_dump failed; no backup was written"
fi
# A readable table of contents proves the archive is complete.
if ! pg_restore --list "$temporary" > /dev/null; then
  rm -f -- "$temporary"
  fail "the new archive is unreadable; no backup was written"
fi
sync "$temporary" 2> /dev/null || sync
mv -- "$temporary" "$target"
echo "backup: wrote $name ($(wc -c < "$target" | tr -d ' ') bytes)"

# Keep the newest BACKUP_RETENTION complete backups. Names sort by time.
ls -1 "$dir" | grep -E '^ai-sdlc-[0-9]{8}T[0-9]{6}Z\.dump$' | sort -r | tail -n "+$((retention + 1))" |
  while IFS= read -r old; do
    rm -f -- "$dir/$old"
    echo "backup: pruned $old"
  done
echo "backup: done"
