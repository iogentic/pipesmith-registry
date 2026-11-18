# Local Changes

`web/` holds the files of `ui/web` of the interface repository at commit 1e6d884, taken over on 2026-11-12. The files in the table were changed here afterwards, and the interface repository can take the same change. `TestEmbeddedFilesAreTheTakenCommitAndTheListedChanges` in `ui_test.go` pins the git blob id of each of them at the taken commit and here.

| File | Date | Reason |
| --- | --- | --- |
| `web/js/preview.js` | 2026-11-18 | After a sign in and after every reload the page waited for all probes, and the probe of the search screen was a search. A search without a hit reads every row of the index, so on a large index the page waited for a walk over all of it. Each entry of `PROBES` now names its read request beside its probe, and the search screen has a request but no probe. `fromAreas` marks the areas from the refused requests of `GET /api/areas` and sends nothing. `probe` marks each area when its answer arrives, keeps a result the screen learned from its own request meanwhile, and drops answers that arrive after a sign out. |
| `web/js/app.js` | 2026-11-18 | The reason is the one of `preview.js`. `loadAreas` marks the areas through `fromAreas` and returns whether the answer came. `start` and `signedIn` start the probes only without that answer, and they no longer wait for them. |

Section Preview of `docs/ui/API.md` in the interface repository still says that the probe of every served read leaves the browser. With this change no probe leaves it while the server answers `GET /api/areas`, which is the alternative that decision U41 of that repository names.
