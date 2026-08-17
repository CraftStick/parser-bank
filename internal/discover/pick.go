package discover

import "strings"

// Здесь выбирается, какое поле за что отвечает.
//
// Раньше выбор шёл первым совпадением по порядку в JSON, и этого оказалось
// мало: порядок ключей случаен, а рядом с нужным полем почти всегда лежит
// похожее, но неправильное — комиссия рядом с суммой, категория рядом с
// контрагентом, идентификатор счёта рядом с идентификатором операции.
// Поэтому кандидаты ранжируются, а не берутся первым попавшимся.

// pickMoney разделяет остаток по счёту и сумму операции.
//
// Имя сверяется с последним сегментом пути: родитель уточняет смысл, но не
// задаёт его. Иначе sections.LIVE_BALANCES.displayOrder сходит за остаток —
// «BALANCES» в пути есть, а числом является порядок сортировки.
func pickMoney(fields []*field) (balance, amount *field) {
	// Первым проходом берём только «главные» суммы, вторым — любые
	// подходящие. Так комиссия не займёт место суммы операции.
	for _, primaryOnly := range []bool{true, false} {
		for _, f := range fields {
			if !f.numeric() {
				continue
			}
			last := f.last()
			if !reAmount.MatchString(last) && !reBalance.MatchString(last) {
				continue
			}
			if primaryOnly && reSecondaryMoney.MatchString(f.Path) {
				continue
			}

			if reBalance.MatchString(f.Path) {
				if balance == nil {
					balance = f
				}
			} else if amount == nil {
				amount = f
			}
		}
		if balance != nil || amount != nil {
			return balance, amount
		}
	}
	return balance, amount
}

// pickDate ищет время операции, игнорируя даты продукта и служебные метки.
//
// Наличие такой даты — главный признак, отличающий список операций от списка
// счетов. Поэтому важно не принять за неё openDate: со счётом это превратило
// бы остаток в «операцию» и сломало опознание обоих.
func pickDate(fields []*field) *field {
	var fallback *field
	for _, f := range fields {
		if !looksLikeDate(f) || reNonOperationDate.MatchString(f.Path) {
			continue
		}
		if reDate.MatchString(f.Path) {
			return f
		}
		if fallback == nil {
			fallback = f
		}
	}
	return fallback
}

// pickID ищет идентификатор записи.
//
// Главное требование — различаться от записи к записи. Идентификатор счёта
// или продукта повторяется у всех операций, и если взять его, все операции
// схлопнутся в одну: бот покажет первую и промолчит про остальные.
func pickID(fields []*field) *field {
	var best *field
	bestRank := 99

	for _, f := range fields {
		if !reID.MatchString(f.Path) {
			continue
		}
		if len(f.strings()) == 0 && !f.numeric() {
			continue
		}
		// Проверка возможна только когда есть с чем сравнивать.
		if len(f.Samples) > 1 && !f.unique() {
			continue
		}

		if r := idRank(f); r < bestRank {
			best, bestRank = f, r
		}
	}
	return best
}

func idRank(f *field) int {
	last := f.last()

	rank := 2
	switch {
	case strings.EqualFold(last, "id"):
		rank = 0
	case reStrongID.MatchString(last):
		rank = 1
	}

	// Вложенный id принадлежит не записи, а её части: parentCategory.id —
	// это идентификатор категории, и у разных операций он вполне может
	// совпасть. Такое поле годится только когда ничего лучше нет.
	if f.depth() > 0 {
		rank += 2
	}
	return rank
}

// pickTitle выбирает, что показать человеку в уведомлении.
//
// Предпочтение — контрагенту и назначению платежа. Категория операции тоже
// подходит под «name», но в уведомлении «Переводы» бесполезно, а «Иванов И.И.»
// осмысленно.
func pickTitle(fields []*field, exclude ...*field) *field {
	skip := func(f *field) bool {
		for _, e := range exclude {
			if e != nil && e == f {
				return true
			}
		}
		return false
	}

	var best *field
	bestRank := 99

	for _, f := range fields {
		if len(f.strings()) == 0 || skip(f) {
			continue
		}

		var rank int
		switch {
		case reStrongTitle.MatchString(f.Path):
			rank = 0
		case reTitle.MatchString(f.Path):
			rank = 2
		default:
			continue
		}
		// Вложенное поле почти всегда описывает не саму запись,
		// а какую-то её часть.
		if f.depth() > 0 {
			rank++
		}

		if rank < bestRank {
			best, bestRank = f, rank
		}
	}
	return best
}

// pickDirection определяет, где записано направление операции.
//
// Строковый вариант (CREDIT/DEBIT) — самый частый, но встречается и булев
// флаг: у ВТБ это debet: true/false.
func pickDirection(fields []*field) (*field, []string) {
	// Строка с подходящим именем.
	for _, f := range fields {
		if looksLikeDirection(f) && reDirection.MatchString(f.Path) {
			return f, directionInValues(f)
		}
	}
	// Строка с подходящими значениями, но нейтральным именем.
	for _, f := range fields {
		if looksLikeDirection(f) {
			return f, directionInValues(f)
		}
	}
	// Булев флаг.
	for _, f := range fields {
		if in, ok := boolDirectionInValues(f); ok {
			return f, in
		}
	}
	return nil, nil
}
