package discordsignup

import (
	"strings"
	"testing"
)

const testPotluckSheet = `,HELPERS:,[ 2 / 12 ],,,,
NAME,FUN,SETUP,TEARDOWN,DRINKS,FOOD,Explain
Ann,TRUE,TRUE,FALSE,TRUE,FALSE,Lemonade
bo ,TRUE,FALSE,FALSE,FALSE,TRUE,
Cy,TRUE,TRUE,TRUE,TRUE,TRUE,"Soda, snacks, and a board game"
Dee,TRUE,TRUE,TRUE,FALSE,FALSE,
Nobody,FALSE,FALSE,FALSE,TRUE,FALSE,Chips
Sam,FALSE,FALSE,FALSE,TRUE,FALSE,Ice
,FALSE,FALSE,FALSE,FALSE,FALSE,
`

// TestASheetRowBecomesOneItem: drinks, food or both make one item, a blank
// explanation names the category, and ticking neither makes none.
func TestASheetRowBecomesOneItem(t *testing.T) {
	rows, err := parsePotluckSheet(strings.NewReader(testPotluckSheet))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 6 {
		t.Fatalf("rows = %d, want 6 named ones", len(rows))
	}
	for _, c := range []struct {
		row            int
		category, text string
		ok             bool
	}{
		{0, PotluckDrinks, "Lemonade", true},
		{1, PotluckFood, "Food", true},
		{2, PotluckDrinks, "Soda, snacks, and a board game", true},
		{3, "", "", false},
	} {
		category, text, ok := rows[c.row].item()
		if category != c.category || text != c.text || ok != c.ok {
			t.Errorf("row %d = %q %q %v, want %q %q %v", c.row, category, text, ok, c.category, c.text, c.ok)
		}
	}
	if _, err := parsePotluckSheet(strings.NewReader("a,b\n1,2\n")); err != ErrSheetLayout {
		t.Errorf("a sheet without the columns: err = %v", err)
	}
}

// TestCopyingASheetLeavesPeoplesOwnItemsAlone: names match only one person
// going, a copied row is copied again only when it changes, and what people
// said themselves is never touched.
func TestCopyingASheetLeavesPeoplesOwnItemsAlone(t *testing.T) {
	store := testStore(t)
	ev := potluckEvent(t, store, "ann", "bo", "cy", "dee", "sam1", "sam2")
	store.db.Exec(`UPDATE signups SET display_name = 'Sam' WHERE discord_user_id IN ('sam1','sam2')`)
	store.db.Exec(`UPDATE signups SET display_name = 'Ann' WHERE discord_user_id = 'ann'`)
	if _, err := store.AddPotluckItem(ev.ID, "ann", PotluckOther, "A speaker"); err != nil {
		t.Fatal(err)
	}
	rows, _ := parsePotluckSheet(strings.NewReader(testPotluckSheet))
	roster, _ := store.Roster(ev.ID, false)

	copyAll := func(rows []sheetRow) (changed int, unmatched []string) {
		for _, row := range rows {
			userID, ok := matchGoing(row.Name, roster)
			if !ok {
				unmatched = append(unmatched, row.Name)
				continue
			}
			did, err := store.copySheetRow(ev.ID, userID, row, nil)
			if err != nil {
				t.Fatal(err)
			}
			if did {
				changed++
			}
		}
		return changed, unmatched
	}
	changed, unmatched := copyAll(rows)
	if changed != 4 || strings.Join(unmatched, ",") != "Nobody,Sam" {
		t.Fatalf("changed %d, unmatched %v; want 4 and Nobody, Sam (two people are called Sam)", changed, unmatched)
	}
	sheet := mustSheet(t, store, ev.ID)
	if got := len(sheet.ItemsOf("ann")); got != 2 {
		t.Fatalf("Ann has %d items, want her own speaker and the copied lemonade", got)
	}
	if sheet.Answered("dee") {
		t.Fatal("Dee ticked nothing but counts as answered")
	}

	if changed, _ := copyAll(rows); changed != 0 {
		t.Fatalf("an unchanged sheet changed %d rows", changed)
	}
	// Bo removes the copied food; an unchanged row does not bring it back.
	bo := mustSheet(t, store, ev.ID).ItemsOf("bo")[0]
	if err := store.RemoveOwnPotluckItem(ev.ID, bo.ID, "bo"); err != nil {
		t.Fatal(err)
	}
	copyAll(rows)
	if len(mustSheet(t, store, ev.ID).ItemsOf("bo")) != 0 {
		t.Fatal("the copy put back an item Bo removed")
	}
	// Ann changes her row: the old copy goes, the new one comes, her own stays.
	rows[0].Explain = "Pink lemonade"
	copyAll(rows)
	var texts []string
	for _, it := range mustSheet(t, store, ev.ID).ItemsOf("ann") {
		texts = append(texts, it.Item)
	}
	if strings.Join(texts, ",") != "A speaker,Pink lemonade" {
		t.Fatalf("Ann's items = %v", texts)
	}
}

// TestASheetImportTakesOnlyAGoogleSheetsExport.
func TestASheetImportTakesOnlyAGoogleSheetsExport(t *testing.T) {
	store := testStore(t)
	ev := potluckEvent(t, store)
	for _, bad := range []string{"http://docs.google.com/spreadsheets/d/x/export?format=csv",
		"https://example.com/spreadsheets/d/x/export?format=csv", "https://docs.google.com/spreadsheets/d/x/edit"} {
		if _, err := store.SetPotluckSheetImport(ev.ID, bad); err == nil {
			t.Errorf("%s was taken", bad)
		}
	}
	imp, err := store.SetPotluckSheetImport(ev.ID, "https://docs.google.com/spreadsheets/d/x/export?format=csv")
	if err != nil || imp.EventID != ev.ID || len(imp.Unmatched) != 0 {
		t.Fatalf("set = %+v, %v", imp, err)
	}
}
