package textutil

import "testing"

func TestHTMLToText(t *testing.T) {
	in := "&lt;h2&gt;About&lt;/h2&gt;&lt;p&gt;We &amp;amp; you.&lt;/p&gt;&lt;ul&gt;&lt;li&gt;Go&lt;/li&gt;&lt;li&gt;SQL&lt;/li&gt;&lt;/ul&gt;"
	want := "About\nWe & you.\n\n- Go\n- SQL"
	if got := HTMLToText(in); got != want {
		t.Errorf("got %q want %q", got, want)
	}
}
