package signup

import "testing"

func TestCheckLogin(t *testing.T) {
	cases := map[string]string{
		"vasya":                             "",
		"vasya.pupkin":                      "",
		"v_p-2026":                          "",
		"abc":                               "",
		"ab":                                ReasonInvalid,
		"1vasya":                            ReasonInvalid,
		"_vasya":                            ReasonInvalid,
		"vasya.":                            ReasonInvalid,
		"va..sya":                           ReasonInvalid,
		"va.-sya":                           ReasonInvalid,
		"вася":                              ReasonInvalid,
		"vasya pupkin":                      ReasonInvalid,
		"Vasya":                             ReasonInvalid, // must be normalized first
		"abcdefghijklmnopqrstuvwxyzabcdefg": ReasonInvalid, // 33
		"postmaster":                        ReasonReserved,
		"admin":                             ReasonReserved,
		"sberbank-help":                     ReasonReserved,
		"my.gosuslugi":                      ReasonReserved,
		"lunlun":                            ReasonReserved,
		"supportteam":                       ReasonReserved,
	}
	for login, want := range cases {
		if got := CheckLogin(login); got != want {
			t.Errorf("CheckLogin(%q) = %q, want %q", login, got, want)
		}
	}
}

func TestNormalizeLogin(t *testing.T) {
	for in, want := range map[string]string{
		" Vasya ":              "vasya",
		"Vasya@LunLun.Email":   "vasya",
		"vasya.pupkin@foo.bar": "vasya.pupkin",
	} {
		if got := NormalizeLogin(in); got != want {
			t.Errorf("NormalizeLogin(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNewPassword(t *testing.T) {
	p, err := NewPassword(16)
	if err != nil || len(p) != 16 {
		t.Fatalf("NewPassword = %q, %v", p, err)
	}
	q, _ := NewPassword(16)
	if p == q {
		t.Fatal("two passwords are equal")
	}
}
