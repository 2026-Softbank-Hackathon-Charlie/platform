package storetest

import (
	"errors"
	"fmt"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

// ConformanceSecrets runs the store.SecretStore contract against fresh
// stores from newStore, with the same rules as Conformance.
func ConformanceSecrets(t *testing.T, newStore func(t *testing.T) store.Store) {
	cases := []struct {
		name string
		run  func(t *testing.T, s store.Store)
	}{
		{"SecretsChangeList", testSecretsChangeList},
		{"SecretsScopes", testSecretsScopes},
		{"SecretsLimit", testSecretsLimit},
		{"SecretsLimitRace", testSecretsLimitRace},
		{"SecretsDeleteApp", testSecretsDeleteApp},
		{"SecretsDeleteBranch", testSecretsDeleteBranch},
		{"SecretsDeleteRace", testSecretsDeleteRace},
		{"SecretsCopiesAndText", testSecretsCopiesAndText},
		{"InstanceStorage", testInstanceStorage},
		{"BackupStorage", testBackupStorage},
		{"AppDatabase", testAppDatabase},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := newStore(t)
			t.Cleanup(s.Close)
			c.run(t, s)
		})
	}
}

// manySecrets is a limit the tests not about the limit never reach.
const manySecrets = 100

func newSecret(name, value string, updatedAt int) store.AppSecret {
	return store.AppSecret{
		Name:       name,
		KeyVersion: 1,
		KeyCheck:   "0a1b2c3d4e5f",
		Nonce:      []byte("nonce-" + name),
		Ciphertext: []byte("sealed:" + value),
		UpdatedAt:  at(updatedAt),
		UpdatedBy:  "seohyun",
	}
}

// stored is s as a read returns it after storing it in app and instance.
func stored(app, instance string, s store.AppSecret) store.AppSecret {
	s.App, s.Instance, s.UpdatedAt = app, instance, pg(s.UpdatedAt)
	return s
}

func secretNames(ss []store.AppSecret) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.Instance + "/" + s.Name
	}
	return out
}

func testSecretsChangeList(t *testing.T, s store.Store) {
	ctx := t.Context()
	for _, app := range []string{"moimroom", "nobody"} {
		list, err := s.ListAppSecrets(ctx, app)
		must(t, err)
		if list == nil || len(list) != 0 {
			t.Fatalf("ListAppSecrets(%s) = %v before any secret, want an empty non-nil slice", app, list)
		}
	}
	must(t, s.CreateApp(ctx, newApp("moimroom", 0)))
	must(t, s.CreateApp(ctx, newApp("shop", 0)))

	host, admin := newSecret("HOST_CODE", "4821", 1), newSecret("ADMIN_TOKEN", "t0ken", 1)
	// App and Instance of the rows are ignored; the arguments name the scope.
	host.App, host.Instance = "shop", "elsewhere"
	must(t, s.ChangeAppSecrets(ctx, "moimroom", "", []store.AppSecret{host, admin}, nil, manySecrets))
	must(t, s.ChangeAppSecrets(ctx, "shop", "", []store.AppSecret{newSecret("SHOP_KEY", "k", 1)}, nil, manySecrets))

	list, err := s.ListAppSecrets(ctx, "moimroom")
	must(t, err)
	eqList(t, "moimroom secrets", secretNames(list), []string{"/ADMIN_TOKEN", "/HOST_CODE"})
	eqFields(t, "HOST_CODE", list[1], stored("moimroom", "", newSecret("HOST_CODE", "4821", 1)))
	got, err := s.GetAppSecret(ctx, "moimroom", "", "ADMIN_TOKEN")
	must(t, err)
	eqFields(t, "ADMIN_TOKEN", got, stored("moimroom", "", admin))

	// Replacing one, deleting another and an absent name in one change.
	newHost := newSecret("HOST_CODE", "9999", 2)
	newHost.UpdatedBy, newHost.KeyVersion, newHost.KeyCheck = "jiwoo", 2, "ffeeddccbbaa"
	must(t, s.ChangeAppSecrets(ctx, "moimroom", "", []store.AppSecret{newHost}, []string{"ADMIN_TOKEN", "NEVER_SET"}, manySecrets))
	list, err = s.ListAppSecrets(ctx, "moimroom")
	must(t, err)
	eqList(t, "moimroom secrets after the change", secretNames(list), []string{"/HOST_CODE"})
	eqFields(t, "replaced HOST_CODE", list[0], stored("moimroom", "", newHost))
	_, err = s.GetAppSecret(ctx, "moimroom", "", "ADMIN_TOKEN")
	wantErr(t, err, store.ErrNotFound)
	_, err = s.GetAppSecret(ctx, "nobody", "", "HOST_CODE")
	wantErr(t, err, store.ErrNotFound)

	// The other app is untouched.
	list, err = s.ListAppSecrets(ctx, "shop")
	must(t, err)
	eqList(t, "shop secrets", secretNames(list), []string{"/SHOP_KEY"})

	wantErr(t, s.ChangeAppSecrets(ctx, "nobody", "", []store.AppSecret{newSecret("A", "b", 3)}, nil, manySecrets), store.ErrNotFound)
}

func testSecretsScopes(t *testing.T, s store.Store) {
	ctx := t.Context()
	must(t, s.CreateApp(ctx, newApp("moimroom", 0)))
	must(t, s.CreateApp(ctx, newApp("shop", 0)))
	must(t, s.CreateAppBranch(ctx, newAppBranch("moimroom", "dev", "moimroom--dev", 0), manyBranches))
	must(t, s.CreateAppBranch(ctx, newAppBranch("shop", "dev", "shop--dev", 0), manyBranches))

	must(t, s.ChangeAppSecrets(ctx, "moimroom", "", []store.AppSecret{newSecret("HOST_CODE", "app", 1), newSecret("B", "app", 1)}, nil, manySecrets))
	// The default instance is the app's name; an added branch its instance.
	must(t, s.ChangeAppSecrets(ctx, "moimroom", "moimroom", []store.AppSecret{newSecret("HOST_CODE", "default", 2)}, nil, manySecrets))
	must(t, s.ChangeAppSecrets(ctx, "moimroom", "moimroom--dev", []store.AppSecret{newSecret("HOST_CODE", "dev", 3), newSecret("A", "dev", 3)}, nil, manySecrets))

	list, err := s.ListAppSecrets(ctx, "moimroom")
	must(t, err)
	// By instance ("" first) then name, in byte order.
	eqList(t, "moimroom secrets", secretNames(list), []string{"/B", "/HOST_CODE", "moimroom/HOST_CODE", "moimroom--dev/A", "moimroom--dev/HOST_CODE"})
	got, err := s.GetAppSecret(ctx, "moimroom", "moimroom--dev", "HOST_CODE")
	must(t, err)
	eqFields(t, "dev HOST_CODE", got, stored("moimroom", "moimroom--dev", newSecret("HOST_CODE", "dev", 3)))
	_, err = s.GetAppSecret(ctx, "moimroom", "moimroom--dev", "B")
	wantErr(t, err, store.ErrNotFound)

	// Deleting an override leaves the app-wide secret.
	must(t, s.ChangeAppSecrets(ctx, "moimroom", "moimroom--dev", nil, []string{"HOST_CODE"}, manySecrets))
	list, err = s.ListAppSecrets(ctx, "moimroom")
	must(t, err)
	eqList(t, "after deleting an override", secretNames(list), []string{"/B", "/HOST_CODE", "moimroom/HOST_CODE", "moimroom--dev/A"})

	// Only the app's own instances are scopes.
	for _, instance := range []string{"shop--dev", "shop", "moimroom--qa"} {
		err := s.ChangeAppSecrets(ctx, "moimroom", instance, []store.AppSecret{newSecret("X", "y", 4)}, nil, manySecrets)
		wantErr(t, err, store.ErrInstanceNotFound)
		wantErr(t, err, store.ErrNotFound)
	}
}

func testSecretsLimit(t *testing.T, s store.Store) {
	ctx := t.Context()
	must(t, s.CreateApp(ctx, newApp("moimroom", 0)))
	must(t, s.ChangeAppSecrets(ctx, "moimroom", "", []store.AppSecret{newSecret("A", "1", 1), newSecret("B", "1", 1)}, nil, 3))
	// The limit counts the scope after the change.
	err := s.ChangeAppSecrets(ctx, "moimroom", "", []store.AppSecret{newSecret("C", "1", 2), newSecret("D", "1", 2)}, nil, 3)
	wantErr(t, err, store.ErrSecretLimit)
	wantErr(t, err, store.ErrConflict)
	list, err := s.ListAppSecrets(ctx, "moimroom")
	must(t, err)
	eqList(t, "after a refused change", secretNames(list), []string{"/A", "/B"})
	// Replacing and deleting in the same change makes room.
	must(t, s.ChangeAppSecrets(ctx, "moimroom", "", []store.AppSecret{newSecret("A", "2", 3), newSecret("C", "1", 3), newSecret("D", "1", 3)}, []string{"B"}, 3))
	list, err = s.ListAppSecrets(ctx, "moimroom")
	must(t, err)
	eqList(t, "after replacing and deleting", secretNames(list), []string{"/A", "/C", "/D"})
	// Each scope has its own limit.
	must(t, s.ChangeAppSecrets(ctx, "moimroom", "moimroom", []store.AppSecret{newSecret("A", "1", 4), newSecret("E", "1", 4), newSecret("F", "1", 4)}, nil, 3))

	// Above a lowered limit, deleting and replacing still work; growing does not.
	must(t, s.ChangeAppSecrets(ctx, "moimroom", "", []store.AppSecret{newSecret("A", "3", 5)}, nil, 1))
	must(t, s.ChangeAppSecrets(ctx, "moimroom", "", nil, []string{"C"}, 1))
	wantErr(t, s.ChangeAppSecrets(ctx, "moimroom", "", []store.AppSecret{newSecret("G", "1", 6)}, nil, 1), store.ErrSecretLimit)
	wantErr(t, s.ChangeAppSecrets(ctx, "moimroom", "", []store.AppSecret{newSecret("G", "1", 6), newSecret("H", "1", 6)}, []string{"D"}, 1), store.ErrSecretLimit)
	must(t, s.ChangeAppSecrets(ctx, "moimroom", "", []store.AppSecret{newSecret("G", "1", 7)}, []string{"D"}, 1))
	list, err = s.ListAppSecrets(ctx, "moimroom")
	must(t, err)
	eqList(t, "after changes above a lowered limit", secretNames(list), []string{"/A", "/G", "moimroom/A", "moimroom/E", "moimroom/F"})
}

// testSecretsLimitRace changes one scope from many goroutines at once: the
// limit holds.
func testSecretsLimitRace(t *testing.T, s store.Store) {
	ctx := t.Context()
	must(t, s.CreateApp(ctx, newApp("moimroom", 0)))
	const n, limit = 8, 3
	errs := make([]error, n)
	parallel(n, func(i int) {
		errs[i] = s.ChangeAppSecrets(ctx, "moimroom", "", []store.AppSecret{newSecret(fmt.Sprintf("S%d", i), "v", i)}, nil, limit)
	})
	wins := 0
	for i, err := range errs {
		switch {
		case err == nil:
			wins++
		case !errors.Is(err, store.ErrSecretLimit):
			t.Fatalf("change %d: %v", i, err)
		}
	}
	list, err := s.ListAppSecrets(ctx, "moimroom")
	must(t, err)
	if wins != limit || len(list) != limit {
		t.Fatalf("%d changes succeeded and %d secrets are stored, want %d", wins, len(list), limit)
	}
}

func testSecretsDeleteApp(t *testing.T, s store.Store) {
	ctx := t.Context()
	must(t, s.CreateApp(ctx, newApp("moimroom", 0)))
	must(t, s.CreateApp(ctx, newApp("shop", 0)))
	must(t, s.CreateAppBranch(ctx, newAppBranch("moimroom", "dev", "moimroom--dev", 0), manyBranches))
	must(t, s.ChangeAppSecrets(ctx, "moimroom", "", []store.AppSecret{newSecret("A", "1", 1)}, nil, manySecrets))
	must(t, s.ChangeAppSecrets(ctx, "moimroom", "moimroom", []store.AppSecret{newSecret("B", "1", 1)}, nil, manySecrets))
	must(t, s.ChangeAppSecrets(ctx, "moimroom", "moimroom--dev", []store.AppSecret{newSecret("C", "1", 1)}, nil, manySecrets))
	must(t, s.ChangeAppSecrets(ctx, "shop", "", []store.AppSecret{newSecret("A", "1", 1)}, nil, manySecrets))

	must(t, s.DeleteApp(ctx, "moimroom"))
	list, err := s.ListAppSecrets(ctx, "moimroom")
	must(t, err)
	if len(list) != 0 {
		t.Fatalf("secrets %v outlived their app", secretNames(list))
	}
	list, err = s.ListAppSecrets(ctx, "shop")
	must(t, err)
	eqList(t, "the other app's secrets", secretNames(list), []string{"/A"})

	// Registering the name again starts with no secrets.
	must(t, s.CreateApp(ctx, newApp("moimroom", 2)))
	list, err = s.ListAppSecrets(ctx, "moimroom")
	must(t, err)
	if len(list) != 0 {
		t.Fatalf("a re-registered app has secrets %v", secretNames(list))
	}
}

func testSecretsDeleteBranch(t *testing.T, s store.Store) {
	ctx := t.Context()
	must(t, s.CreateApp(ctx, newApp("moimroom", 0)))
	dev := newAppBranch("moimroom", "dev", "moimroom--dev", 0)
	must(t, s.CreateAppBranch(ctx, dev, manyBranches))
	must(t, s.CreateAppBranch(ctx, newAppBranch("moimroom", "qa", "moimroom--qa", 0), manyBranches))
	must(t, s.ChangeAppSecrets(ctx, "moimroom", "", []store.AppSecret{newSecret("A", "1", 1)}, nil, manySecrets))
	must(t, s.ChangeAppSecrets(ctx, "moimroom", "moimroom", []store.AppSecret{newSecret("A", "2", 1)}, nil, manySecrets))
	must(t, s.ChangeAppSecrets(ctx, "moimroom", "moimroom--dev", []store.AppSecret{newSecret("A", "3", 1)}, nil, manySecrets))
	must(t, s.ChangeAppSecrets(ctx, "moimroom", "moimroom--qa", []store.AppSecret{newSecret("A", "4", 1)}, nil, manySecrets))

	must(t, s.DeleteAppBranch(ctx, "moimroom", "dev"))
	list, err := s.ListAppSecrets(ctx, "moimroom")
	must(t, err)
	eqList(t, "after deleting dev", secretNames(list), []string{"/A", "moimroom/A", "moimroom--qa/A"})
	wantErr(t, s.ChangeAppSecrets(ctx, "moimroom", "moimroom--dev", []store.AppSecret{newSecret("A", "5", 2)}, nil, manySecrets), store.ErrInstanceNotFound)

	// Adding the branch again starts with no overrides.
	must(t, s.CreateAppBranch(ctx, dev, manyBranches))
	list, err = s.ListAppSecrets(ctx, "moimroom")
	must(t, err)
	eqList(t, "after adding dev again", secretNames(list), []string{"/A", "moimroom/A", "moimroom--qa/A"})
}

// testSecretsDeleteRace changes secrets while the branch, then the app, is
// deleted: no secret outlives its instance or app.
func testSecretsDeleteRace(t *testing.T, s store.Store) {
	ctx := t.Context()
	must(t, s.CreateApp(ctx, newApp("moimroom", 0)))
	must(t, s.CreateAppBranch(ctx, newAppBranch("moimroom", "dev", "moimroom--dev", 0), manyBranches))

	const n = 8
	errs := make([]error, n+1)
	parallel(n+1, func(i int) {
		if i == n {
			errs[i] = s.DeleteAppBranch(ctx, "moimroom", "dev")
			return
		}
		err := s.ChangeAppSecrets(ctx, "moimroom", "moimroom--dev", []store.AppSecret{newSecret(fmt.Sprintf("S%d", i), "v", i)}, nil, manySecrets)
		if !errors.Is(err, store.ErrInstanceNotFound) {
			errs[i] = err
		}
	})
	for _, err := range errs {
		must(t, err)
	}
	list, err := s.ListAppSecrets(ctx, "moimroom")
	must(t, err)
	if len(list) != 0 {
		t.Fatalf("overrides %v outlived their instance", secretNames(list))
	}

	errs = make([]error, n+1)
	parallel(n+1, func(i int) {
		if i == n {
			errs[i] = s.DeleteApp(ctx, "moimroom")
			return
		}
		instance := []string{"", "moimroom"}[i%2]
		err := s.ChangeAppSecrets(ctx, "moimroom", instance, []store.AppSecret{newSecret(fmt.Sprintf("S%d", i), "v", i)}, nil, manySecrets)
		if !errors.Is(err, store.ErrNotFound) {
			errs[i] = err
		}
	})
	for _, err := range errs {
		must(t, err)
	}
	list, err = s.ListAppSecrets(ctx, "moimroom")
	must(t, err)
	if len(list) != 0 {
		t.Fatalf("secrets %v outlived their app", secretNames(list))
	}
}

func testSecretsCopiesAndText(t *testing.T, s store.Store) {
	ctx := t.Context()
	must(t, s.CreateApp(ctx, newApp("moimroom", 0)))
	sec := newSecret("A\x00B", "v", 1)
	sec.UpdatedBy = "seo\x00hyun\xff"
	must(t, s.ChangeAppSecrets(ctx, "moimroom", "", []store.AppSecret{sec}, nil, manySecrets))
	// The caller's slices are not kept.
	sec.Nonce[0], sec.Ciphertext[0] = 'X', 'X'
	got, err := s.GetAppSecret(ctx, "moimroom", "", "AB")
	must(t, err)
	want := stored("moimroom", "", newSecret("AB", "v", 1))
	want.Nonce, want.UpdatedBy = []byte("nonce-A\x00B"), "seohyun�"
	eqFields(t, "cleaned secret", got, want)
	// A read's slices are its own.
	got.Ciphertext[0] = 'X'
	again, err := s.GetAppSecret(ctx, "moimroom", "", "AB")
	must(t, err)
	eqFields(t, "secret after changing a read", again, want)
	list, err := s.ListAppSecrets(ctx, "moimroom")
	must(t, err)
	list[0].Nonce[0] = 'X'
	again, err = s.GetAppSecret(ctx, "moimroom", "", "AB")
	must(t, err)
	eqFields(t, "secret after changing a listed one", again, want)

	// Lookups by keys that are not clean find nothing.
	_, err = s.GetAppSecret(ctx, "moimroom", "", "A\x00B")
	wantErr(t, err, store.ErrNotFound)
	_, err = s.GetAppSecret(ctx, "moim\x00room", "", "AB")
	wantErr(t, err, store.ErrNotFound)
	list, err = s.ListAppSecrets(ctx, "moim\x00room")
	must(t, err)
	if len(list) != 0 {
		t.Fatalf("an unclean app name lists %v", secretNames(list))
	}
	wantErr(t, s.ChangeAppSecrets(ctx, "moim\x00room", "", nil, []string{"AB"}, manySecrets), store.ErrNotFound)
	wantErr(t, s.ChangeAppSecrets(ctx, "moimroom", "moim\x00room", nil, []string{"AB"}, manySecrets), store.ErrInstanceNotFound)
	// Deleting by an unclean name deletes nothing.
	must(t, s.ChangeAppSecrets(ctx, "moimroom", "", nil, []string{"A\x00B"}, manySecrets))
	if _, err := s.GetAppSecret(ctx, "moimroom", "", "AB"); err != nil {
		t.Fatalf("an unclean name deleted the clean one: %v", err)
	}
}

func newInstanceStorage(instance, env, app string, createdAt int) store.InstanceStorage {
	return store.InstanceStorage{
		Instance: instance, Env: env, App: app, Driver: "aws", Bucket: "hopcharlie-" + env + "-" + instance,
		KeyID: "AKIA" + instance, SealedAccessKey: "AgB1c2Vk" + instance, SealedSecretKey: "AgBzZWNy" + instance,
		CertFingerprint: "SHA256:abc", CreatedAt: at(createdAt),
	}
}

func storageKeys(ss []store.InstanceStorage) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.Instance + "@" + s.Env
	}
	return out
}

func testInstanceStorage(t *testing.T, s store.Store) {
	ctx := t.Context()
	list, err := s.ListInstanceStorage(ctx, "moimroom", "")
	must(t, err)
	if list == nil || len(list) != 0 {
		t.Fatalf("ListInstanceStorage before any row = %v, want an empty non-nil slice", list)
	}
	// No app is needed: the rows outlive it.
	rows := []store.InstanceStorage{
		newInstanceStorage("moimroom--dev", "eks", "moimroom", 1),
		newInstanceStorage("moimroom", "onprem", "moimroom", 2),
		newInstanceStorage("moimroom", "eks", "moimroom", 3),
		newInstanceStorage("shop", "eks", "shop", 4),
	}
	rows[1].Driver = "minio"
	for _, r := range rows {
		must(t, s.PutInstanceStorage(ctx, r))
	}
	list, err = s.ListInstanceStorage(ctx, "moimroom", "")
	must(t, err)
	eqList(t, "moimroom storage", storageKeys(list), []string{"moimroom@eks", "moimroom@onprem", "moimroom--dev@eks"})
	list, err = s.ListInstanceStorage(ctx, "", "moimroom")
	must(t, err)
	eqList(t, "instance moimroom", storageKeys(list), []string{"moimroom@eks", "moimroom@onprem"})
	list, err = s.ListInstanceStorage(ctx, "moimroom", "moimroom--dev")
	must(t, err)
	eqList(t, "app and instance", storageKeys(list), []string{"moimroom--dev@eks"})
	list, err = s.ListInstanceStorage(ctx, "shop", "moimroom")
	must(t, err)
	eqList(t, "another app's instance", storageKeys(list), []string{})
	list, err = s.ListInstanceStorage(ctx, "", "")
	must(t, err)
	eqList(t, "all rows", storageKeys(list), []string{"moimroom@eks", "moimroom@onprem", "moimroom--dev@eks", "shop@eks"})
	want := rows[1]
	want.CreatedAt = pg(want.CreatedAt)
	eqFields(t, "moimroom@onprem", list[1], want)
	got, err := s.GetInstanceStorage(ctx, "moimroom", "onprem")
	must(t, err)
	eqFields(t, "get moimroom@onprem", got, want)
	_, err = s.GetInstanceStorage(ctx, "moimroom", "gke")
	wantErr(t, err, store.ErrNotFound)

	// Put replaces the row, a new key sealed with a new certificate, and
	// keeps when it was first made.
	r := newInstanceStorage("moimroom", "eks", "moimroom", 5)
	r.KeyID, r.OldKeyID, r.SealedAccessKey, r.SealedSecretKey, r.CertFingerprint = "AKIANEW", r.KeyID, "AgBuZXc=", "AgBuZXcy", "SHA256:def"
	must(t, s.PutInstanceStorage(ctx, r))
	got, err = s.GetInstanceStorage(ctx, "moimroom", "eks")
	must(t, err)
	r.CreatedAt = pg(at(3))
	eqFields(t, "replaced moimroom@eks", got, r)

	// Deleting the app does not delete the rows.
	must(t, s.CreateApp(ctx, newApp("moimroom", 0)))
	must(t, s.DeleteApp(ctx, "moimroom"))
	list, err = s.ListInstanceStorage(ctx, "moimroom", "")
	must(t, err)
	if len(list) != 3 {
		t.Fatalf("storage rows after deleting the app = %v, want all 3", storageKeys(list))
	}

	must(t, s.DeleteInstanceStorage(ctx, "moimroom", "eks"))
	wantErr(t, s.DeleteInstanceStorage(ctx, "moimroom", "eks"), store.ErrNotFound)
	_, err = s.GetInstanceStorage(ctx, "moimroom", "eks")
	wantErr(t, err, store.ErrNotFound)
	list, err = s.ListInstanceStorage(ctx, "moimroom", "")
	must(t, err)
	eqList(t, "after delete", storageKeys(list), []string{"moimroom@onprem", "moimroom--dev@eks"})

	// Strings are cleaned, and unclean keys find nothing.
	dirty := newInstanceStorage("moim\x00room--x", "e\x00ks", "moimroom", 6)
	dirty.KeyID = "AKIA\xff"
	must(t, s.PutInstanceStorage(ctx, dirty))
	got, err = s.GetInstanceStorage(ctx, "moimroom--x", "eks")
	must(t, err)
	if got.KeyID != "AKIA�" || got.Bucket != "hopcharlie-eks-moimroom--x" {
		t.Fatalf("cleaned row %+v", got)
	}
	_, err = s.GetInstanceStorage(ctx, "moim\x00room--x", "e\x00ks")
	wantErr(t, err, store.ErrNotFound)
	wantErr(t, s.DeleteInstanceStorage(ctx, "moim\x00room--x", "e\x00ks"), store.ErrNotFound)
	list, err = s.ListInstanceStorage(ctx, "moim\x00room", "")
	must(t, err)
	if len(list) != 0 {
		t.Fatalf("an unclean app lists %v", storageKeys(list))
	}
}

// testBackupStorage checks the backup bucket rows (ADR 0025): by app and
// environment, apart from the instance storage rows, and kept when the app
// is deleted.
func testBackupStorage(t *testing.T, s store.Store) {
	ctx := t.Context()
	_, err := s.GetBackupStorage(ctx, "moimroom", "eks")
	wantErr(t, err, store.ErrNotFound)
	r := newInstanceStorage("moimroom---pg", "eks", "moimroom", 1)
	must(t, s.PutBackupStorage(ctx, r))
	got, err := s.GetBackupStorage(ctx, "moimroom", "eks")
	must(t, err)
	want := r
	want.CreatedAt = pg(want.CreatedAt)
	eqFields(t, "moimroom backup@eks", got, want)
	_, err = s.GetBackupStorage(ctx, "moimroom", "gke")
	wantErr(t, err, store.ErrNotFound)
	// Not an instance's bucket: the cleanup of deleted instances lists
	// instance storage only.
	_, err = s.GetInstanceStorage(ctx, "moimroom---pg", "eks")
	wantErr(t, err, store.ErrNotFound)
	list, err := s.ListInstanceStorage(ctx, "moimroom", "")
	must(t, err)
	eqList(t, "instance storage with a backup row", storageKeys(list), []string{})

	// Put replaces the row and keeps when it was first made.
	r2 := newInstanceStorage("moimroom---pg", "eks", "moimroom", 2)
	r2.KeyID, r2.OldKeyID, r2.CertFingerprint = "AKIANEW", r.KeyID, "SHA256:def"
	must(t, s.PutBackupStorage(ctx, r2))
	got, err = s.GetBackupStorage(ctx, "moimroom", "eks")
	must(t, err)
	r2.CreatedAt = pg(at(1))
	eqFields(t, "replaced moimroom backup@eks", got, r2)

	// Deleting the app keeps the row.
	must(t, s.CreateApp(ctx, newApp("moimroom", 0)))
	must(t, s.DeleteApp(ctx, "moimroom"))
	_, err = s.GetBackupStorage(ctx, "moimroom", "eks")
	must(t, err)

	// Unclean keys find nothing.
	_, err = s.GetBackupStorage(ctx, "moim\x00room", "eks")
	wantErr(t, err, store.ErrNotFound)
}

func newAppDatabase(primary, peer string, createdAt int) store.AppDatabase {
	return store.AppDatabase{
		PrimaryEnv: primary, PeerEnv: peer, KeyVersion: 1, KeyCheck: "0a1b2c3d4e5f",
		PasswordNonce: []byte("nonce-pw"), PasswordCiphertext: []byte("sealed:pw"),
		CACert:     "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n",
		CAKeyNonce: []byte("nonce-key"), CAKeyCiphertext: []byte("sealed:key"),
		CreatedAt: at(createdAt),
	}
}

func testAppDatabase(t *testing.T, s store.Store) {
	ctx := t.Context()
	if _, err := s.GetAppDatabase(ctx, "moimroom-mig"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetAppDatabase of nothing = %v, want ErrNotFound", err)
	}
	list, err := s.ListAppDatabases(ctx)
	must(t, err)
	if list == nil || len(list) != 0 {
		t.Fatalf("ListAppDatabases of nothing = %#v, want empty and not nil", list)
	}

	want := newAppDatabase("gke", "eks", 1)
	want.App = "ignored: the app's name wins"
	must(t, s.CreateAppWithDatabase(ctx, newApp("moimroom-mig", 0), want))
	if _, err := s.GetApp(ctx, "moimroom-mig"); err != nil {
		t.Fatalf("the app of CreateAppWithDatabase: %v", err)
	}
	got, err := s.GetAppDatabase(ctx, "moimroom-mig")
	must(t, err)
	want.App, want.CreatedAt = "moimroom-mig", pg(want.CreatedAt)
	eqFields(t, "app database", got, want)
	got.PasswordCiphertext[0] = 'X'
	again, err := s.GetAppDatabase(ctx, "moimroom-mig")
	must(t, err)
	eqFields(t, "app database after changing a read copy", again, want)

	// A refused pair stores neither the app nor the pair.
	for _, envs := range [][2]string{{"eks", "eks"}, {"", "eks"}, {"gke", ""}} {
		err := s.CreateAppWithDatabase(ctx, newApp("shop", 0), newAppDatabase(envs[0], envs[1], 1))
		if err == nil {
			t.Fatalf("CreateAppWithDatabase with environments %q succeeded", envs)
		}
		if _, err := s.GetApp(ctx, "shop"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("app shop after a refused database pair %q: %v, want ErrNotFound", envs, err)
		}
		if _, err := s.GetAppDatabase(ctx, "shop"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("database pair of shop after a refused one %q: %v, want ErrNotFound", envs, err)
		}
	}

	// A taken name stores no pair.
	must(t, s.CreateApp(ctx, newApp("shop", 0)))
	wantErr(t, s.CreateAppWithDatabase(ctx, newApp("shop", 0), newAppDatabase("gke", "eks", 1)), store.ErrConflict)
	if _, err := s.GetAppDatabase(ctx, "shop"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("database pair of an app registered before: %v, want ErrNotFound", err)
	}

	must(t, s.CreateAppWithDatabase(ctx, newApp("blog", 0), newAppDatabase("eks", "gke", 2)))
	list, err = s.ListAppDatabases(ctx)
	must(t, err)
	var apps []string
	for _, d := range list {
		apps = append(apps, d.App)
	}
	eqList(t, "app databases", apps, []string{"blog", "moimroom-mig"})

	// DeleteApp removes the pair; registering the name again has none.
	must(t, s.DeleteApp(ctx, "moimroom-mig"))
	if _, err := s.GetAppDatabase(ctx, "moimroom-mig"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("database pair after DeleteApp: %v, want ErrNotFound", err)
	}
	must(t, s.CreateApp(ctx, newApp("moimroom-mig", 3)))
	if _, err := s.GetAppDatabase(ctx, "moimroom-mig"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("database pair of a re-registered app: %v, want ErrNotFound", err)
	}
	if _, err := s.GetAppDatabase(ctx, "blog"); err != nil {
		t.Fatalf("the other app's pair after DeleteApp: %v", err)
	}
}
