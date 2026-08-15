# Fake Steam Deck Home

This tree is synthetic test data. It mirrors only the current path shapes needed by discovery and security tests and contains no real account, plugin, theme or artwork data.

The fixture intentionally uses the fake home owner `casey` rather than `deck` so tests cannot accidentally depend on a hardcoded SteamOS username. Numeric Steam account and application IDs are reserved fixture values, not copied user identifiers.

Expected supported roots:

- `homebrew/plugins`, `homebrew/settings`, and `homebrew/data` for a regular Decky plugin;
- `homebrew/themes` for the CSS Loader special adapter;
- `.local/share/Steam/userdata/<account>/config/grid` for account artwork;
- selected files below `.local/share/Steam/appcache/librarycache` for Steam-game icon behavior.

`shortcuts.vdf` is a textual safety placeholder. Any future binary-VDF tests must use a purpose-built synthetic fixture and must never source a real user file.
