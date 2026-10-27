package homepath

import "testing"

func TestFold(t *testing.T) {
	t.Setenv("HOME", "/home/dev")
	tests := []struct{ in, want string }{
		{"/home/dev", "~"},
		{"/home/dev/projects/app", "~/projects/app"},
		{"/home/developer/app", "/home/developer/app"},
		{"/var/lib/x", "/var/lib/x"},
		{"relative/path", "relative/path"},
	}
	for _, tt := range tests {
		if got := Fold(tt.in); got != tt.want {
			t.Errorf("Fold(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestFoldText(t *testing.T) {
	t.Setenv("HOME", "/home/dev")
	tests := []struct{ in, want string }{
		{"reading /home/dev/.config/x: denied", "reading ~/.config/x: denied"},
		{"home is /home/dev", "home is ~"},
		{`path="/home/dev/a" other=/home/developer/b`, `path="~/a" other=/home/developer/b`},
		{"lock file /home/dev/a and /home/dev/b", "lock file ~/a and ~/b"},
		{"prefix/home/dev/x", "prefix/home/dev/x"},
		{"nothing here", "nothing here"},
	}
	for _, tt := range tests {
		if got := FoldText(tt.in); got != tt.want {
			t.Errorf("FoldText(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestNoHome(t *testing.T) {
	t.Setenv("HOME", "")
	if Fold("/x") != "/x" || FoldText("/x") != "/x" {
		t.Fatal("folded without a home")
	}
	t.Setenv("HOME", "/")
	if Fold("/x") != "/x" || FoldText("/x") != "/x" {
		t.Fatal("folded the root directory")
	}
}
