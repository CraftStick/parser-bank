package bank

import (
	"context"
	"slices"
	"testing"
)

func TestResolvableFiltersDeadDomainsAndKeepsOrder(t *testing.T) {
	// .invalid — зарезервированный TLD, он гарантированно не резолвится,
	// поэтому тест не зависит от того, какие зеркала живы сегодня.
	hosts := []string{
		"parserbank-nonexistent-1.invalid",
		"127.0.0.1",
		"parserbank-nonexistent-2.invalid",
		"localhost",
	}

	got := Resolvable(context.Background(), hosts)
	want := []string{"127.0.0.1", "localhost"}

	if !slices.Equal(got, want) {
		t.Errorf("Resolvable = %v, ожидалось %v", got, want)
	}
}

func TestResolvableEmptyInput(t *testing.T) {
	if got := Resolvable(context.Background(), nil); len(got) != 0 {
		t.Errorf("на пустом входе получено %v", got)
	}
}

func TestNewMirrorFinderFallsBackToDefaults(t *testing.T) {
	f := NewMirrorFinder(nil, nil)
	if !slices.Equal(f.Candidates(), DefaultMirrors) {
		t.Errorf("без списка ожидались зеркала по умолчанию, получено %v", f.Candidates())
	}

	custom := []string{"online.example.ru"}
	if got := NewMirrorFinder(nil, custom).Candidates(); !slices.Equal(got, custom) {
		t.Errorf("свой список не применился: %v", got)
	}
}

func TestCandidatesReturnsCopy(t *testing.T) {
	// Список отдаётся наружу в команду /mirrors — вызывающий не должен уметь
	// испортить состояние finder-а.
	f := NewMirrorFinder(nil, []string{"a", "b"})

	got := f.Candidates()
	got[0] = "испорчено"

	if f.Candidates()[0] != "a" {
		t.Error("Candidates отдал ссылку на внутренний срез")
	}
}
