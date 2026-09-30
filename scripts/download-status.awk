function readable(bytes,    unit) {
	unit = 0
	while (bytes >= 1024 && unit < 4) {
		bytes /= 1024
		unit++
	}
	return sprintf("%.1f %s", bytes, units[unit])
}

BEGIN {
	units[0] = "Б"
	units[1] = "КиБ"
	units[2] = "МиБ"
	units[3] = "ГиБ"
	units[4] = "ТиБ"
}

/^fias_download_progress_(downloaded|total)_bytes\{/ {
	line = $0
	match(line, /kind="[^"]+"/)
	kind = substr(line, RSTART + 6, RLENGTH - 7)
	match(line, /version_id="[^"]+"/)
	version = substr(line, RSTART + 12, RLENGTH - 13)
	key = kind SUBSEP version
	if (line ~ /^fias_download_progress_downloaded_bytes\{/) {
		downloaded[key] = $2
		active[key] = 1
	} else {
		total[key] = $2
	}
}

END {
	for (key in active) {
		split(key, parts, SUBSEP)
		if (total[key] > 0) {
			printf "%s, версия %s: %s / %s (%.1f%%)\n", parts[1], parts[2], readable(downloaded[key]), readable(total[key]), downloaded[key] / total[key] * 100
		} else {
			printf "%s, версия %s: загружено %s (полный размер неизвестен)\n", parts[1], parts[2], readable(downloaded[key])
		}
		count++
	}
	if (!count) print "Сейчас активных загрузок нет."
}
