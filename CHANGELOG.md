# Changelog

All notable changes to argus are documented here.

## [0.2.0] - 2026-08-01

### Bug Fixes
- Fail fast when a worker pane wedges, not hang forever (#399) ([`716f3df`](https://github.com/Elysium-Labs-EU/argus/commit/716f3dfbbdd28f69aaa30ebd3a74393d9daf6233))
- Allow planning self-transition to fill an empty plan (#400) ([`7845523`](https://github.com/Elysium-Labs-EU/argus/commit/784552336463bb74ff8275c02d1b4d7564e67e82))
- Reject --max-rounds <= 0 instead of silently defaulting (#401) ([`f00960a`](https://github.com/Elysium-Labs-EU/argus/commit/f00960a72da98538c0200727697b3297624a68e2))


### Testing
- Cover DefaultOwnerLabel, Write/Load/Enforce error paths (#380) ([`d984b83`](https://github.com/Elysium-Labs-EU/argus/commit/d984b83c90b3316f5de8f1e484961677d8334ee6))
- Cover SettingsPath and write/error paths (#381) ([`8992dcf`](https://github.com/Elysium-Labs-EU/argus/commit/8992dcfb78d67b741195add5dfcb3797ff2cc8f9))
- Cover Write/Load error paths to reach 80% coverage (#382) ([`4fa1f1c`](https://github.com/Elysium-Labs-EU/argus/commit/4fa1f1ccc2e4da6970c9ec443538064d82851794))
- Cover Confirm and renderSpinner in internal/ui (#398) ([`ace23e1`](https://github.com/Elysium-Labs-EU/argus/commit/ace23e1b3fb3868d9fcfaefad16b544e0685d802))

## [0.2.0-rc.13] - 2026-08-01

### Bug Fixes
- Pre-approve MCP consent, detect idle no-report hangs (#374) ([`99b5754`](https://github.com/Elysium-Labs-EU/argus/commit/99b57545cb4c2902529ead338013ede344968332))
- Pre-authorize the sanctioned force-push (#375) ([`2d9b9ad`](https://github.com/Elysium-Labs-EU/argus/commit/2d9b9adf3be74ac42d3e4a4eb736e1be6306a2bc))
- Don't hard-escalate a zero-diff self-report fix (#376) ([`8f70182`](https://github.com/Elysium-Labs-EU/argus/commit/8f70182239775c7c0489fff37dd3b78e7b2586ac))
- Worker never commits/pushes; argus does (#378) ([`a98b87d`](https://github.com/Elysium-Labs-EU/argus/commit/a98b87d95c88ff306db0aeb7041d4f6cc9489524))


### Documentation
- Sync argus SKILL.md and README with shipped flags (#372) ([`607bc25`](https://github.com/Elysium-Labs-EU/argus/commit/607bc25932eb3d6fd778730d50ab67854d6ed687))


### Features
- Join tokens with outcome, add model/effort to tokens event (#377) ([`0bc01cb`](https://github.com/Elysium-Labs-EU/argus/commit/0bc01cbc529b3321e5e2d913e7bc5f80c3fb3ae2))

## [0.2.0-rc.12] - 2026-07-31

### CI/CD
- Add SonarQube Cloud scan workflow (#368) ([`5f60b76`](https://github.com/Elysium-Labs-EU/argus/commit/5f60b76ece1a2b5b71e826224b9c23ba385d8208))


### Features
- Preflight config commands before spawn (#358) ([`898d0a9`](https://github.com/Elysium-Labs-EU/argus/commit/898d0a9401b7fabef2aa14f019b22d589279bd1e))
- Bound rework with a persisted cross-invocation restart budget (#359) ([`75f8c5f`](https://github.com/Elysium-Labs-EU/argus/commit/75f8c5f86415f77e6faebfca1d578d213fc024d5))
- Add argus tend to poll a shipped PR's CI checks (#363) ([`64fb73e`](https://github.com/Elysium-Labs-EU/argus/commit/64fb73ea67f4521f1b3cebfe6003de1093e5ab23))
- Deny raw herdr pane mutation on config check --write (#365) ([`95503e7`](https://github.com/Elysium-Labs-EU/argus/commit/95503e7c1e0f475649793818472d1347a0d8d668))
- Add status_page override for self-hosted forges (#361) ([`5a541d0`](https://github.com/Elysium-Labs-EU/argus/commit/5a541d024729802ec07b6e9434883f768847fae6))

## [0.2.0-rc.11] - 2026-07-31

### Bug Fixes
- No-op-round gate ignored HEAD moving on a real rework commit (#324) ([`72d7363`](https://github.com/Elysium-Labs-EU/argus/commit/72d73638e39c26b23f1101be64324e1056f43208))
- Enforce release-signature verification now that v0.2.0-rc.10 shipped signed (#325) ([`25b0d18`](https://github.com/Elysium-Labs-EU/argus/commit/25b0d18ff189d80624e8771196e4d7a9f72750eb))
- Error on task/branch mismatch, cap branch names (#328) ([`0c763ed`](https://github.com/Elysium-Labs-EU/argus/commit/0c763ed641c38f2fafe551dce00281b32d48ab6b))
- Hint at manual-install fallback on signature refusal (#330) ([`65efbc2`](https://github.com/Elysium-Labs-EU/argus/commit/65efbc2428fae23f5b5fabed13d165786bc0f0dd))
- Reuse existing PR on ship retry instead of duplicating (#337) ([`31f6af7`](https://github.com/Elysium-Labs-EU/argus/commit/31f6af74bdba16757f9526e98a9f77cd01384ed4))
- Reject paragraph-shaped --tasks-file input before spawning workers (#336) ([`a9e1522`](https://github.com/Elysium-Labs-EU/argus/commit/a9e15228b79346413d052cbe37e88d48a8690b87))
- Strip trailing parenthetical asides from replayed Cmd (#346) ([`583865f`](https://github.com/Elysium-Labs-EU/argus/commit/583865fc6eba03679e1fb74aebe44721d145cbeb))
- Run replayed test cmd from Target subdir when it exists (#351) ([`b2595d6`](https://github.com/Elysium-Labs-EU/argus/commit/b2595d67bca7106092ad45977275084709bc5254))
- Close Jira double-notify gap, document checkpoint/resume contract (#355) ([`0b9cf3b`](https://github.com/Elysium-Labs-EU/argus/commit/0b9cf3bf8923c6381537057a90e3e94110e9343e))
- Soften shell-parse failures in VerifyTests to a waivable reason (#356) ([`454be54`](https://github.com/Elysium-Labs-EU/argus/commit/454be54405a0a2511ec2c8ef69ab063615dd7c9b))
- Don't require an origin ref for a zero-divergence branch (#357) ([`87b9bcb`](https://github.com/Elysium-Labs-EU/argus/commit/87b9bcb363d12904834077ee5eac8b22a9a64c9f))


### Features
- File-based ownership lease for supervised worktrees (#349) ([`b5dab08`](https://github.com/Elysium-Labs-EU/argus/commit/b5dab086b2075d3949ac09313352b4509caf98ba))
- Let a supervisor steer a working worker mid-turn (#354) ([`1dbadfd`](https://github.com/Elysium-Labs-EU/argus/commit/1dbadfd3d3b0e27d2096479c07577b14d02c8847))
- Harmonize config-key/CLI-flag naming and close two review-gate safety gaps (#344) ([`d138017`](https://github.com/Elysium-Labs-EU/argus/commit/d1380178ca93b3087f21eb52f7554dd0e2d12353))


### Maintenance
- Bump softprops/action-gh-release from 2 to 3 (#319) ([`0ed16f1`](https://github.com/Elysium-Labs-EU/argus/commit/0ed16f17d8f04a0dec63de5db3ebba879d211730))
- Bump actions/download-artifact from 7 to 8 (#320) ([`cf2e5af`](https://github.com/Elysium-Labs-EU/argus/commit/cf2e5af4ff8ee566c8079780a5754ca9fa2dac36))
- Bump actions/upload-artifact from 6 to 7 (#322) ([`794b490`](https://github.com/Elysium-Labs-EU/argus/commit/794b490e60d51e4f508842fa7a7e01dbc8da4f5b))
- Bump the go-dependencies group with 2 updates (#321) ([`d2ad045`](https://github.com/Elysium-Labs-EU/argus/commit/d2ad0459de579a674e7a6dac93263b959f22cf6b))
- Add typos, verify, file-size, interface{} gates (#353) ([`03b6dbe`](https://github.com/Elysium-Labs-EU/argus/commit/03b6dbe8365199ab48e23a145a3ba961104050fe))


### Refactoring
- Share --worktree/--base/--repo/--verify-cmd flag binding (#352) ([`9abece7`](https://github.com/Elysium-Labs-EU/argus/commit/9abece78d14592082ba39582426322869ca2fa84))

## [0.2.0-rc.10] - 2026-07-28

### Bug Fixes
- Provision real release-signing keypair, embed matching pubkey (#323) ([`c91a17c`](https://github.com/Elysium-Labs-EU/argus/commit/c91a17c48de97c583305e86b142aa9a72c2e0d03))

## [0.2.0-rc.9] - 2026-07-28

### Bug Fixes
- Tighten local-state file perms and stamp audit-log actor (#297) ([`46050d5`](https://github.com/Elysium-Labs-EU/argus/commit/46050d58af52cb72736a4eb6d6d15f371d53f28e))
- Stop --attach reusing a stale prior-verdict baseline (#317) ([`ac38a95`](https://github.com/Elysium-Labs-EU/argus/commit/ac38a955e189bdd65b618c311875adf082fe351c))
- Skip appending label-shaped test Target to rerun Cmd (#318) ([`bb73321`](https://github.com/Elysium-Labs-EU/argus/commit/bb733214afa90e9fb4bdc9ebf8e0f9060dd71f9d))


### Features
- Allow tests[] entries to mark expected/intentional failures (#316) ([`1d83223`](https://github.com/Elysium-Labs-EU/argus/commit/1d8322390cce34918d70bcd22d5a18397b286a69))
- Add dependabot, gitleaks, and govulncheck to CI (#298) ([`ddcefc2`](https://github.com/Elysium-Labs-EU/argus/commit/ddcefc263c689df47353a37f2c60b6c898f3d235))

## [0.2.0-rc.8] - 2026-07-28

### Bug Fixes
- Replay worker test/verify commands argv-style, not via sh -c (#296) ([`ff63604`](https://github.com/Elysium-Labs-EU/argus/commit/ff636042617c04af1adcb8c915fb3660c5758880))
- Exclude binary untracked files from MeasureDiff line counts (#306) ([`9caa190`](https://github.com/Elysium-Labs-EU/argus/commit/9caa19003c6182c01d6fc8df6be312530362ac2d))


### Features
- Hint at forge status page on request/push failures (#295) ([`c7e22d7`](https://github.com/Elysium-Labs-EU/argus/commit/c7e22d7d6f559ba6b673179f0d95938f482ca1a1))
- Mechanically enforce repo title_prefix_template at ship time (#307) ([`c77ff39`](https://github.com/Elysium-Labs-EU/argus/commit/c77ff396874ad92c48f6c79e3ba85d7eb37e8716))
- Add launcher key to .argus/config.yml (#308) ([`5ef5c13`](https://github.com/Elysium-Labs-EU/argus/commit/5ef5c13bcc4b6cf699c5a55b65f5498c8bc59eee))
- Add repo-configurable worktree_setup_cmd bootstrap hook (#309) ([`8367e7d`](https://github.com/Elysium-Labs-EU/argus/commit/8367e7d18d7180e7ed893968c3490212753959dd))

## [0.2.0-rc.7] - 2026-07-28

### Bug Fixes
- Merge issue-fetched branch defaults into correct slots (#294) ([`d7edf5b`](https://github.com/Elysium-Labs-EU/argus/commit/d7edf5bdf17fd7be1384b1fea831eef42e4f1ad7))
- Worker diff_stat instruction now matches gate's base ref (#310) ([`84070c1`](https://github.com/Elysium-Labs-EU/argus/commit/84070c1796e41bef13562159cc86f98c4a46545c))

## [0.2.0-rc.6] - 2026-07-27

### Bug Fixes
- Escalate no-op rework rounds (#288) ([`3f62e47`](https://github.com/Elysium-Labs-EU/argus/commit/3f62e475bf5dff9b447b1c98212e6f7c7614cfe5))


### Features
- Surface per-worker approval provenance and verify-once contract (#286) ([`3bb3c62`](https://github.com/Elysium-Labs-EU/argus/commit/3bb3c62efd5d6baf301c0c6194a4fb248b6fe443))
- Repeatable --findings and --findings-file for argus rework (#289) ([`ce796a2`](https://github.com/Elysium-Labs-EU/argus/commit/ce796a28dfc322596dfccd89760ff252df94e086))

## [0.2.0-rc.5] - 2026-07-27

### Bug Fixes
- Stop mis-shaping gate's replay of worker-claimed test commands (#273) ([`60939e2`](https://github.com/Elysium-Labs-EU/argus/commit/60939e2a65b8583e622aaacf2d4718298b02ca63))
- Preserve PR title across rework's InvalidateStatus reset (#284) ([`ab88881`](https://github.com/Elysium-Labs-EU/argus/commit/ab888819a28e773a59f4848392753b20d35663d8))


### Features
- Structured blocked questions with `argus worker answer` (#279) ([`9f831ae`](https://github.com/Elysium-Labs-EU/argus/commit/9f831aee69f50dc20ea12139232af83c94d225e6))
- Interactive shell-completion installer for argus completion (#281) ([`790a079`](https://github.com/Elysium-Labs-EU/argus/commit/790a079767b5d60763f9ebc35ac5e0868e6e3d49))


### Refactoring
- Split runShip and runWorktreePrune to cut CRAP score (#274) ([`b79fc40`](https://github.com/Elysium-Labs-EU/argus/commit/b79fc40937d2f061c3cdd72a7a47131668979903))
- Extract sub-steps to drop CRAP under 20 (#275) ([`0862ff3`](https://github.com/Elysium-Labs-EU/argus/commit/0862ff30ec81a6f0344d97d373cbffdb41a66364))
- Cut CRAP score of reconcile and checkHerdrStuck (#276) ([`abea0eb`](https://github.com/Elysium-Labs-EU/argus/commit/abea0eb10e628a1e24aa7347cc7459a10f34007b))
- Extract allow-entry merge and settings write from Ensure (#277) ([`e5967e0`](https://github.com/Elysium-Labs-EU/argus/commit/e5967e00ddcbba959f40c2cf1b8a99c77e7d69fc))
- Extract parseYAML scalar-key switch into helper (#278) ([`406b593`](https://github.com/Elysium-Labs-EU/argus/commit/406b5936a2d0e7937f7bca0938a99b8b4c94733a))

## [0.2.0-rc.4] - 2026-07-27

### Bug Fixes
- Surface herdr permission-prompt blocks during rebase/rework waits (#237) ([`341b627`](https://github.com/Elysium-Labs-EU/argus/commit/341b627c240e4c48b4f1d54cb6e565da39c23bfd))
- Allow awaiting_review to self-recover to working (#238) ([`d20ab89`](https://github.com/Elysium-Labs-EU/argus/commit/d20ab89a87b442b0c5c58373226b39810828ce01))
- Gate must not check diff under-report before terminal phase (#250) ([`de44e78`](https://github.com/Elysium-Labs-EU/argus/commit/de44e78b02b848797ed8a63b0695ad58b422b149))
- Nudge herdr-done worker before escalating to a human (#251) ([`7b22346`](https://github.com/Elysium-Labs-EU/argus/commit/7b2234666c8cd1d590e02d51bccad11178d80a32))
- Log transcript count on missing plan-evidence verdict (#252) ([`c7e968b`](https://github.com/Elysium-Labs-EU/argus/commit/c7e968b5c73bcbf753d1d3ffa914a49f674b389a))
- Allowlist forges by host, require --forge for the rest (#254) ([`fbcab53`](https://github.com/Elysium-Labs-EU/argus/commit/fbcab534e10646cd12d2aa504ecc1fd716698e42))
- Glab token fallback uses config get, not nonexistent auth token (#257) ([`5b7f7ec`](https://github.com/Elysium-Labs-EU/argus/commit/5b7f7ec8394335aa953c10e92f50895fae50d0ee))
- Capture ship-gate subprocess output via a real file, not a pipe (#264) ([`4eb0907`](https://github.com/Elysium-Labs-EU/argus/commit/4eb0907808217ce6e8fcafe76468d175295611be))
- VerifyTests join Cmd and Target when re-running claimed test pass (#265) ([`0d7cfc4`](https://github.com/Elysium-Labs-EU/argus/commit/0d7cfc49380accb11980ee8432dc9a766646dc33))


### Features
- Add config.schema.json + LSP header for .argus/config.yml (#246) ([`6ecdb66`](https://github.com/Elysium-Labs-EU/argus/commit/6ecdb663a3639a964667521e2dfea9d1985f7f0a))
- Make worker worktree location configurable via worktree_dir (#249) ([`0dd9bb2`](https://github.com/Elysium-Labs-EU/argus/commit/0dd9bb2518969bedf4403d9c8b7be34145680986))
- Assign/transition Jira issues at supervise spawn time (#253) ([`a4032c4`](https://github.com/Elysium-Labs-EU/argus/commit/a4032c477f4c340d15f960710e8798d8eace1e3f))
- Forge config key + --forge on supervise/worktree prune (#258) ([`178999f`](https://github.com/Elysium-Labs-EU/argus/commit/178999f95623f1e6bfff571554ee2916f20a252c))


### Maintenance
- Trim external-precedent aside from check-schema-sync comment (#247) ([`c334c20`](https://github.com/Elysium-Labs-EU/argus/commit/c334c206c6c7cfe637cb789e3b42c5412ceb2052))
- Gitignore the locally-built argus binary (#255) ([`ce2df67`](https://github.com/Elysium-Labs-EU/argus/commit/ce2df67ffa3e2996916d1701229f4f5fc6ccb051))
- Tighten go-crap gate threshold from 30 to 20 (#262) ([`580c653`](https://github.com/Elysium-Labs-EU/argus/commit/580c6532b67598c09e8ed07c0bf6886a657ff627))
- Sweep issue-number citations from comments (#263) ([`d0e2979`](https://github.com/Elysium-Labs-EU/argus/commit/d0e297913a78c46c0d7d8916f7d2ff7b24e5c4a8))

## [0.2.0-rc.3] - 2026-07-27

### Bug Fixes
- Argus-fix-issue-229 (#229) (#230) ([`b2fd6bd`](https://github.com/Elysium-Labs-EU/argus/commit/b2fd6bdc107c284c05d28874af1b128bf06bdbd8))
- Argus-fix-issue-226 (#226) (#231) ([`7915c75`](https://github.com/Elysium-Labs-EU/argus/commit/7915c75d962f8dfdc11a55ef5fb514aa3f1c2db1))
- Argus-fix-issue-227 (#227) (#232) ([`6024381`](https://github.com/Elysium-Labs-EU/argus/commit/602438111aac79ff511878ca6cb1747005f353b9))
- Argus-fix-issue-225 (#225) (#233) ([`d96146f`](https://github.com/Elysium-Labs-EU/argus/commit/d96146f284a5de3c488cbe63d9639dae9f8a4271))
- Argus-fix-issue-228 (#228) (#234) ([`b478532`](https://github.com/Elysium-Labs-EU/argus/commit/b478532d4604c1717a57806964b03aca76986191))

## [0.2.0-rc.2] - 2026-07-26

### Bug Fixes
- Argus-fix-issue-211 (#211) (#215) ([`4433990`](https://github.com/Elysium-Labs-EU/argus/commit/44339901a04660fb73162d06cda3927d00146d4e))
- Argus-fix-issue-212 (#212) (#213) ([`1f4862b`](https://github.com/Elysium-Labs-EU/argus/commit/1f4862be2da0369c5b04c63502f65c311e649c87))
- Argus-fix-issue-210 (#210) (#217) ([`69f85ce`](https://github.com/Elysium-Labs-EU/argus/commit/69f85ce4a50e7c3814338dc614489540caa13b3c))
- Argus-fix-issue-209 (#209) (#218) ([`f9bd118`](https://github.com/Elysium-Labs-EU/argus/commit/f9bd118f946136502871e525728a2101c24e64c5))
- Argus-fix-issue-214 (#214) (#219) ([`5a1ce24`](https://github.com/Elysium-Labs-EU/argus/commit/5a1ce246a2a40cc1f2bdbee8f329a8de6d163006))
- Argus-fix-issue-216 (#216) (#220) ([`ca96863`](https://github.com/Elysium-Labs-EU/argus/commit/ca9686301fc197f470766251cac4f9a003a1c5c6))
- Argus-fix-issue-222 (#222) (#223) ([`8bd366c`](https://github.com/Elysium-Labs-EU/argus/commit/8bd366c06f84ba22875abb17784cac0497e3cb8a))
- Argus-fix-issue-216 (#224) ([`f8ea5db`](https://github.com/Elysium-Labs-EU/argus/commit/f8ea5db9a2b54987c029cb79547dd1fcaa046b49))


### Documentation
- Rewrite README around architecture, LLMs, setup, tools (#221) ([`7e0e7a6`](https://github.com/Elysium-Labs-EU/argus/commit/7e0e7a6ebefd6fe2acfd0f860472c840955d5596))

## [0.2.0-rc.1] - 2026-07-25

### Bug Fixes
- Argus-fix-issue-207 (#207) (#208) ([`2814e23`](https://github.com/Elysium-Labs-EU/argus/commit/2814e23460f2d196fb54f3f3f53afd690b946b38))

## [0.1.0] - 2026-07-25

### Bug Fixes
- Argus-fix-issue-177 (#177) (#179) ([`c488681`](https://github.com/Elysium-Labs-EU/argus/commit/c48868119d5933da1d4410f36cdfa6f97b77741b))
- Argus-fix-issue-178 (#178) (#180) ([`2463333`](https://github.com/Elysium-Labs-EU/argus/commit/2463333908814adc854ab36e00aa5100fda798b8))
- Argus-fix-issue-182 (#182) (#188) ([`5f37c1a`](https://github.com/Elysium-Labs-EU/argus/commit/5f37c1ab925d7f0696449809e7302f0a9c9fe384))
- Argus-fix-issue-184 (#184) (#189) ([`f4a957b`](https://github.com/Elysium-Labs-EU/argus/commit/f4a957b4ce71fbc7154eb26086a1a4e7a7349a3f))
- Argus-fix-issue-185 (#185) (#190) ([`f7c0fdd`](https://github.com/Elysium-Labs-EU/argus/commit/f7c0fddf5570fbc704dfd529a52969d5777f81d5))
- Argus-fix-issue-186 (#186) (#191) ([`57355e8`](https://github.com/Elysium-Labs-EU/argus/commit/57355e887f8138fb8ef774f609509008e02c7dc4))
- Argus-fix-issue-181 (#181) (#192) ([`12a0cde`](https://github.com/Elysium-Labs-EU/argus/commit/12a0cde58a435f70335638789f167e1818b7ba29))
- Argus-fix-issue-183 (#183) (#193) ([`bf1d3b3`](https://github.com/Elysium-Labs-EU/argus/commit/bf1d3b308b063c10302dd2aee3fdb14b0cd128cb))
- Argus-fix-issue-187 (#187) (#199) ([`1ae5e23`](https://github.com/Elysium-Labs-EU/argus/commit/1ae5e2372da420dfd6adc7faeab484e782097668))
- Argus-fix-issue-198 (#198) (#202) ([`8cb58a8`](https://github.com/Elysium-Labs-EU/argus/commit/8cb58a806981053b4abc5cce54d7eb0a9c5e660e))
- Argus-fix-issue-194 (#194) (#200) ([`6734463`](https://github.com/Elysium-Labs-EU/argus/commit/6734463a7ecb91c2fbc73eb7f89efe6e36f55560))
- Argus-fix-issue-196 (#196) (#201) ([`7a74b36`](https://github.com/Elysium-Labs-EU/argus/commit/7a74b369d800e347f7494a68fb9427ca132b7e40))
- Argus-fix-issue-195 (#195) (#197) ([`665953e`](https://github.com/Elysium-Labs-EU/argus/commit/665953eb77a82a40fbeb484b6423d6fbe34fbb88))
- Argus-fix-issue-203 (#203) (#204) ([`7ccd43b`](https://github.com/Elysium-Labs-EU/argus/commit/7ccd43b23ca12d111416a1e1fa401020671e9cc6))
- Argus-fix-issue-205 (#205) (#206) ([`feb6ff5`](https://github.com/Elysium-Labs-EU/argus/commit/feb6ff5b088489e0dc7afcac213eed83bf338d5e))

## [0.1.0-rc.26] - 2026-07-24

### Bug Fixes
- Argus-fix-issue-162-v2 (#162) (#165) ([`2b0064f`](https://github.com/Elysium-Labs-EU/argus/commit/2b0064fea71060e18c1f72c482b734e97d1bb559))
- Argus-fix-issue-161-v2 (#161) (#164) ([`443bc42`](https://github.com/Elysium-Labs-EU/argus/commit/443bc42dd1e81c2a98830a6535b28019bf0a7684))
- Argus-fix-issue-172 (#172) (#173) ([`174ec7c`](https://github.com/Elysium-Labs-EU/argus/commit/174ec7caf3576c28c3c16a8cf403272224b4dc2a))
- Argus-fix-issue-166 (#166) (#169) ([`662b9a8`](https://github.com/Elysium-Labs-EU/argus/commit/662b9a8ed725a2198d15000559269e38415343a3))
- Argus-fix-issue-168 (#168) (#170) ([`3c0674a`](https://github.com/Elysium-Labs-EU/argus/commit/3c0674aec5ed30f1e798873f6aeaab39d1d33f6f))
- Argus-fix-issue-161-v2 (#161) (#164) (#171) ([`6e8ee75`](https://github.com/Elysium-Labs-EU/argus/commit/6e8ee756092c4fd5c0762700e3c1936f7ed4ec6f))

## [0.1.0-rc.25] - 2026-07-24

### Bug Fixes
- Argus-fix-issue-159-v2 (#159) (#163) ([`8f297b5`](https://github.com/Elysium-Labs-EU/argus/commit/8f297b55ecb86e2be708da76fce403d447a446db))

## [0.1.0-rc.24] - 2026-07-24

### Bug Fixes
- Fix-issue-143 (#143) (#144) ([`d193b0a`](https://github.com/Elysium-Labs-EU/argus/commit/d193b0a7df6d87845e5c4ffbb642ec483e38b336))
- Fix-issue-145 (#145) (#147) ([`4134aff`](https://github.com/Elysium-Labs-EU/argus/commit/4134aff1abca0f430273fa4bd94a1033c15fccb6))
- Fix-issue-149 (#149) (#152) ([`3fc45b8`](https://github.com/Elysium-Labs-EU/argus/commit/3fc45b84fed144a3bd55fa87c0c9859449daf400))
- Scope a seam around Claude-Code-specific pieces (fix-issue-146) (#153) ([`c83f35f`](https://github.com/Elysium-Labs-EU/argus/commit/c83f35f86b00cb73858403575e5eded80b525a3f))
- Fix-issue-148 (#148) (#154) ([`35d14d1`](https://github.com/Elysium-Labs-EU/argus/commit/35d14d129dba907c36fff93ffd984d31b8ac4e33))
- Fix-issue-151 (#151) (#155) ([`a275f37`](https://github.com/Elysium-Labs-EU/argus/commit/a275f37babe512917dcb4ddb6ee772f2687a1910))
- Fix-issue-150 (#150) (#156) ([`6a645f2`](https://github.com/Elysium-Labs-EU/argus/commit/6a645f2048f583a5c85c61c52a7c62aae79e4e00))

## [0.1.0-rc.23] - 2026-07-24

### Bug Fixes
- Fix-issue-48 (#48) (#125) ([`46ec3d1`](https://github.com/Elysium-Labs-EU/argus/commit/46ec3d171ec0bb2043b83f25cc333323a2ff1044))
- Scope worktree prune's stash check to the branch being pruned (#137) ([`7cd50a3`](https://github.com/Elysium-Labs-EU/argus/commit/7cd50a3c1066965ec8489f5c8ea6082a4854cc93))
- Fix-issue-135 (#135) (#141) ([`ff38a40`](https://github.com/Elysium-Labs-EU/argus/commit/ff38a4063951fd605847710d227079a9aeefbb5b))
- Fix-issue-140 (#140) (#142) ([`36b84c4`](https://github.com/Elysium-Labs-EU/argus/commit/36b84c4e6d811d66b8b5cd4272327a8004033533))


### CI/CD
- Enable golangci-lint's modernize linter (#139) ([`8d0cc7c`](https://github.com/Elysium-Labs-EU/argus/commit/8d0cc7cd25e48ef1b5ff1b9d09c57966bb433ddb))

## [0.1.0-rc.22] - 2026-07-23

### Bug Fixes
- Fix-issue-116 (#116) ([`4ebad1f`](https://github.com/Elysium-Labs-EU/argus/commit/4ebad1fbbf80ea728d2079e3da4c916f5e1ad0a8))
- Fix-issue-116 (#116) ([`eab6de5`](https://github.com/Elysium-Labs-EU/argus/commit/eab6de5623050d5f4227177338da62e40749e769))
- Fix-issue-102 (#102) ([`dd1c7af`](https://github.com/Elysium-Labs-EU/argus/commit/dd1c7afc2572f93a8fa9b87c24a8c516513d30ff))
- Fix-issue-105 (#105) ([`0ff98ea`](https://github.com/Elysium-Labs-EU/argus/commit/0ff98ea84a84edf7943ee158125343e4070c4977))
- Argus-fix-issue-124 (#124) ([`410bfd5`](https://github.com/Elysium-Labs-EU/argus/commit/410bfd5e5e3c11a954dcc98a957df578b453dde2))
- Fix-issue-109 (#109) ([`c83a506`](https://github.com/Elysium-Labs-EU/argus/commit/c83a5065928b0a73fc85fcd82a10304d14e1c5f9))
- Fix-issue-100 (#100) ([`baae4c6`](https://github.com/Elysium-Labs-EU/argus/commit/baae4c6341b3f25e216eb18a673563a7573f6865))
- Fix-issue-108 (#108) ([`549a5e9`](https://github.com/Elysium-Labs-EU/argus/commit/549a5e99b5628a5289237f195c977e7f9dd4623c))
- Argus-fix-issue-122 (#122) ([`69de591`](https://github.com/Elysium-Labs-EU/argus/commit/69de5914c84b53ae757315ebf3e045a3862045ff))
- Argus-fix-issue-129 (#129) ([`b410530`](https://github.com/Elysium-Labs-EU/argus/commit/b410530d2ac09b724e6b37f98eae32ba0e91a59b))
- Argus-fix-issue-123 (#123) ([`e650fcc`](https://github.com/Elysium-Labs-EU/argus/commit/e650fcc7697dabc3a422c9a001fab90291122cd8))
- Fix-issue-104 (#104) (#119) ([`f47fa61`](https://github.com/Elysium-Labs-EU/argus/commit/f47fa61ac7dd5ffeaa6cad358c81ba46d3fc33bb))
- Argus-fix-issue-128 (#128) (#134) ([`95d31da`](https://github.com/Elysium-Labs-EU/argus/commit/95d31da03972441473eba73aa8fb5ccda05bbbad))


### Features
- Add argus worktree prune for post-ship worktree cleanup ([`fdc90b5`](https://github.com/Elysium-Labs-EU/argus/commit/fdc90b53317f051893288307ba1e30cc1d50dbe5))

## [0.1.0-rc.21] - 2026-07-23

### Bug Fixes
- Fix-issue-107 (#107) ([`f1b8657`](https://github.com/Elysium-Labs-EU/argus/commit/f1b8657d5f1b5968eb41f71b6159e6f620d83528))


### Documentation
- Update argus SKILL.md for rc.20 behavior and known gaps ([`6d407a5`](https://github.com/Elysium-Labs-EU/argus/commit/6d407a550afe79979f3d78d0146890fb2554fca9))
- Add STYLE.md ([`5a514f3`](https://github.com/Elysium-Labs-EU/argus/commit/5a514f34c52aeba31256f174611e406b1cd46d12))

## [0.1.0-rc.20] - 2026-07-23

### Bug Fixes
- Fix-issue-103 (#103) ([`e6572e9`](https://github.com/Elysium-Labs-EU/argus/commit/e6572e982f098e85dc2ac3b89524cdb666234ead))

## [0.1.0-rc.19] - 2026-07-22

### Bug Fixes
- Fix-issue-96 (#96) ([`27ca32a`](https://github.com/Elysium-Labs-EU/argus/commit/27ca32afb7205725e97c05979cc467b681b7cf89))
- Fix-issue-98 (#98) ([`f823869`](https://github.com/Elysium-Labs-EU/argus/commit/f8238696ac3a00d0427ba5f3d4b6f9893c501456))

## [0.1.0-rc.18] - 2026-07-22

### Bug Fixes
- Fix-issue-94 (#94) ([`615c0d8`](https://github.com/Elysium-Labs-EU/argus/commit/615c0d85715137d1e6231577724f69e79928bfb3))


### Documentation
- Document the worker report state machine in README ([`94be6c2`](https://github.com/Elysium-Labs-EU/argus/commit/94be6c22f396c75b9f7ce8ee596873ae283ca0f9))

## [0.1.0-rc.17] - 2026-07-22

### Bug Fixes
- Fix-issue-92 (#92) ([`ba3d200`](https://github.com/Elysium-Labs-EU/argus/commit/ba3d2000931927a50a2a5d16c4acb2c7c67d2661))

## [0.1.0-rc.16] - 2026-07-22

### Bug Fixes
- Fix-issue-90 (#90) ([`301ceae`](https://github.com/Elysium-Labs-EU/argus/commit/301ceae69ef8903d039ca530127870f8bb234aef))
- Don't crash go-crap gate on entries missing coverage key ([`b2944c2`](https://github.com/Elysium-Labs-EU/argus/commit/b2944c2e55e4c22ce6a09b859c1f503dbaa7b4ba))
- Treat equal mtime as fresh, not stale, in isStale ([`9667214`](https://github.com/Elysium-Labs-EU/argus/commit/966721433694873d712fb25610bf8c968b3b727b))

## [0.1.0-rc.15] - 2026-07-22

### Bug Fixes
- Fix-issue-88 (#88) ([`0c4bfd6`](https://github.com/Elysium-Labs-EU/argus/commit/0c4bfd62efa5f453bc66849b3d84ba37e3273ec7))

## [0.1.0-rc.14] - 2026-07-22

### Bug Fixes
- Fix-issue-78 (#78) ([`5e221a6`](https://github.com/Elysium-Labs-EU/argus/commit/5e221a6c77fb84fa67f683e4128f0c01cc0e3d5c))
- Fix-issue-84 (#84) ([`bfb72a7`](https://github.com/Elysium-Labs-EU/argus/commit/bfb72a75d2fd921b343c7e400d15cfc449e12ca2))
- Fix-issue-85 (#85) ([`d8a7bdb`](https://github.com/Elysium-Labs-EU/argus/commit/d8a7bdbeaa5be69aacfa15b974eab9c27a0b721c))
- Fix-issue-79 (#79) ([`66919c8`](https://github.com/Elysium-Labs-EU/argus/commit/66919c897bf1fe462a747f5924957b05f0b2af49))

## [0.1.0-rc.13] - 2026-07-22

### Bug Fixes
- Fix-issue-74 (#74) ([`f278b75`](https://github.com/Elysium-Labs-EU/argus/commit/f278b7516717db44f54c0866ef7780038ccbab73))
- Fix-issue-75 (#75) ([`4f978bc`](https://github.com/Elysium-Labs-EU/argus/commit/4f978bcdfba89c361032f01a7e1c76fbf6f51605))

## [0.1.0-rc.12] - 2026-07-22

### Bug Fixes
- Install.sh must run under real POSIX sh, not just bash (#73) ([`b55750c`](https://github.com/Elysium-Labs-EU/argus/commit/b55750c56fb6faad9eb2fdad9fe3fae0169f3dfd))

## [0.1.0-rc.11] - 2026-07-22

### Bug Fixes
- Correctly pick the newest release in the 'latest' fallback (#72) ([`5821a19`](https://github.com/Elysium-Labs-EU/argus/commit/5821a1969d772467fb99085d1029ad7980deec69))

## [0.1.0-rc.10] - 2026-07-22

### Bug Fixes
- Install.sh's default 'latest' 404s, and the fallback SIGPIPEs (#71) ([`a8a72f6`](https://github.com/Elysium-Labs-EU/argus/commit/a8a72f6cf0fdb3a33edff47e1ab2c4093d4511ea))

## [0.1.0-rc.9] - 2026-07-22

### Bug Fixes
- Resign darwin binary after final install placement (#66) (#70) ([`969674f`](https://github.com/Elysium-Labs-EU/argus/commit/969674f1155e422474bfbef2ed10292dd5627784))

## [0.1.0-rc.8] - 2026-07-22

### Bug Fixes
- Fix-issue-54 (#54) (#59) ([`5278d2e`](https://github.com/Elysium-Labs-EU/argus/commit/5278d2e0631ff2338e4ebc91e7d7e7dd7d8326bb))
- Fix-issue-56 (#56) (#61) ([`3889e1c`](https://github.com/Elysium-Labs-EU/argus/commit/3889e1c27135284ce9dbd253182c51be07593f6c))
- Fix-issue-55 (#55) (#60) ([`77e4bd7`](https://github.com/Elysium-Labs-EU/argus/commit/77e4bd781a8f35bcaa5d5792135e161ea75a8b8d))
- Fix-issue-57 (#57) (#62) ([`44e0a3e`](https://github.com/Elysium-Labs-EU/argus/commit/44e0a3ea3d3c88705776e4bd008f7b064688de3f))
- Fix-issue-58 (#58) (#63) ([`d8c02a1`](https://github.com/Elysium-Labs-EU/argus/commit/d8c02a16fdaf818b5b354b198b7669eff397eee7))
- Fix-issue-64 (generalize credential resolution: any agent key, overridable via CLI flags/config) (#65) ([`52e7294`](https://github.com/Elysium-Labs-EU/argus/commit/52e7294bd81aa8a6659c894db5e0faa9f48df1ae))
- Fix-issue-68 (resolve --repo to an absolute path) (#69) ([`6a52721`](https://github.com/Elysium-Labs-EU/argus/commit/6a527217f9c0bedee538914c2eacf69ae8687271))


### Miscellaneous
- Warn that standalone verdict is not persisted for ship ([`597a752`](https://github.com/Elysium-Labs-EU/argus/commit/597a752c288d6e54b7fac5f3a4f125ac81baa281))

## [0.1.0-rc.7] - 2026-07-21

### Bug Fixes
- Resolve api.atlassian.com cloudId for granular-scoped tokens (#53) ([`03ee75c`](https://github.com/Elysium-Labs-EU/argus/commit/03ee75c9e9faa619cc01c8f92d9e48d93040d405))

## [0.1.0-rc.6] - 2026-07-21

### Bug Fixes
- Fix-issue-50 (#50) (#51) ([`c4da56c`](https://github.com/Elysium-Labs-EU/argus/commit/c4da56c7b6c17211e24f2ac6e88cedd89910f06f))

## [0.1.0-rc.5] - 2026-07-21

### Bug Fixes
- Fix-issue-37 (#37) (#42) ([`4395924`](https://github.com/Elysium-Labs-EU/argus/commit/43959240c8833d15fb7491a490df4e3c82d29e9f))
- Fix-issue-38 (#38) (#43) ([`7353716`](https://github.com/Elysium-Labs-EU/argus/commit/7353716ee6dd6eb1482ba8fdcb4ffdb5b1afcc65))
- Fix-issue-39 (#39) (#44) ([`9d1f84a`](https://github.com/Elysium-Labs-EU/argus/commit/9d1f84aae52d29b20c8be89ce090ecbb14b55bc2))
- Fix-issue-40 (#40) (#45) ([`37166dd`](https://github.com/Elysium-Labs-EU/argus/commit/37166dd6e7e2ae7a72fddf73aa4b458e29ab4c13))
- Fix-issue-41 (#41) (#46) ([`b6681aa`](https://github.com/Elysium-Labs-EU/argus/commit/b6681aa77efed042a2887d7608dc744c75c166c7))
- Fix-issue-47 (#47) (#49) ([`6ba1a55`](https://github.com/Elysium-Labs-EU/argus/commit/6ba1a5577b795fa10be918878ff146c6aa09fa85))

## [0.1.0-rc.4] - 2026-07-21

### Bug Fixes
- Fix-issue-20 (#20) (#30) ([`a9d5d3d`](https://github.com/Elysium-Labs-EU/argus/commit/a9d5d3d93c18faf56fbb0e82f72e16c2d2f27901))
- Fix-issue-21 (#21) (#31) ([`ec545f8`](https://github.com/Elysium-Labs-EU/argus/commit/ec545f88686349c50c06d8f4ef4bc0d2a67b2e3e))
- Fix-issue-22 (#22) (#32) ([`66605e5`](https://github.com/Elysium-Labs-EU/argus/commit/66605e503329d543e7d757f285b94c1a77f24f07))
- Fix-issue-23 (#23) (#33) ([`9cd8f72`](https://github.com/Elysium-Labs-EU/argus/commit/9cd8f72af73003de75b34a570e9f9ef21b5a13e1))
- Fix-issue-24 (#24) (#34) ([`e4d0b02`](https://github.com/Elysium-Labs-EU/argus/commit/e4d0b02fc83391dd938cdf0ec4682ff7786bd35e))
- Fix-issue-25 (#25) (#35) ([`d9bcad0`](https://github.com/Elysium-Labs-EU/argus/commit/d9bcad0f068363af4e79445c49c172517da02d3d))
- Pass --cwd to worktree open, matching worktree create (#36) ([`99d59b7`](https://github.com/Elysium-Labs-EU/argus/commit/99d59b7a35d3379bed833b3d60c143fb36e66bea))

## [0.1.0-rc.3] - 2026-07-21

### Features
- Add argus system update/uninstall, matching eos/theia/themis (#28) ([`d8b04c4`](https://github.com/Elysium-Labs-EU/argus/commit/d8b04c4833b9cff02861e58c24cfab56cbc6c1dd))

## [0.1.0-rc.2] - 2026-07-21

### Bug Fixes
- --issues/--jira-issues alone should satisfy the worker-source guard ([`a22a608`](https://github.com/Elysium-Labs-EU/argus/commit/a22a60836a68acfb1eafd8b275bc235c3eac1112))
- Resolve launcher to an absolute path before spawning (#29) ([`43d663d`](https://github.com/Elysium-Labs-EU/argus/commit/43d663dc559146d67580cf865a5c578f0a0beb20))

## [0.1.0-rc.1] - 2026-07-21

### Bug Fixes
- Run claude -p reviewer inside the worktree with read-only tools ([`8a6f5f8`](https://github.com/Elysium-Labs-EU/argus/commit/8a6f5f8d53abd22bf52932465de09b6638bcaec7))
- Single-quote worktree path + validate branch names ([`626b69d`](https://github.com/Elysium-Labs-EU/argus/commit/626b69d643483ccc26abe5f6afbf88f1aef2eab6))
- Count untracked files in the measured diff ([`8d45e1f`](https://github.com/Elysium-Labs-EU/argus/commit/8d45e1f1cd2a485fb55a246270a549a801b71372))
- Clean forwarded path, skip proxy in --attach mode ([`ce1851d`](https://github.com/Elysium-Labs-EU/argus/commit/ce1851dcd91983f10c839f4355dbba751507f62c))
- Require explicit --base when using supervise --attach ([`8d8e767`](https://github.com/Elysium-Labs-EU/argus/commit/8d8e7672082a6fde86ce0c5b5d432862ad11b878))
- Gate escalates when measured diff is empty despite claimed completion ([`0650544`](https://github.com/Elysium-Labs-EU/argus/commit/065054457a6c5de8e0d1485db850dfc65c17ccf8))
- Only require ast-grep when rules/ actually exists ([`bb53569`](https://github.com/Elysium-Labs-EU/argus/commit/bb53569ed6556c30afd086bbcf70ecead0c79ce3))


### CI/CD
- Add OSV scan (PR-paths + weekly cron + prebuilt pinned binary) ([`e6d5ca3`](https://github.com/Elysium-Labs-EU/argus/commit/e6d5ca3b450446028f05071a34f35f31a669f11e))
- Keep actions/checkout at v7 (current latest) ([`17f4970`](https://github.com/Elysium-Labs-EU/argus/commit/17f4970752505c702e997908a65eac325725fd81))
- Run make ci on every push and PR to main ([`fe76dfd`](https://github.com/Elysium-Labs-EU/argus/commit/fe76dfdb3d9a78d41de5e3087c866a58e90fd1ff))
- Add release pipeline (checksums only, no signing key yet) ([`c005913`](https://github.com/Elysium-Labs-EU/argus/commit/c0059135824d33803a4ec454df6bd8199fb3d307))


### Documentation
- Point herdr links at its GitHub repo ([`ea07142`](https://github.com/Elysium-Labs-EU/argus/commit/ea0714244b81851f214e758f24bb5ce74403906c))
- Add Claude Code skill for driving argus ([`a43ad8f`](https://github.com/Elysium-Labs-EU/argus/commit/a43ad8f18f348058762e4e9b1ead20cee73570ea))


### Features
- Argus deterministic supervisor for herdr worker panes ([`9053a6a`](https://github.com/Elysium-Labs-EU/argus/commit/9053a6ac6a77d02021a60f3aa7074e15b46125cd))
- Post-merge rebase dispatch, claude -p review, PR shipping, go-crap gate ([`4ace013`](https://github.com/Elysium-Labs-EU/argus/commit/4ace0132eba04861634ba2ba976e112162caad0b))
- Typed action logging + reviewer parse re-ask ([`7a9b901`](https://github.com/Elysium-Labs-EU/argus/commit/7a9b90137c673bd71e0849c69a726618fbee0859))
- Verify diff against git ground truth, not worker self-report ([`0bdb5f5`](https://github.com/Elysium-Labs-EU/argus/commit/0bdb5f5a89f13683cfae5fbeb3e680e71c12e155))
- Per-worker deadline + surface unreadable status ([`79298f4`](https://github.com/Elysium-Labs-EU/argus/commit/79298f43f8a228678532b9ee3a110aec090e93b9))
- Enforce argus verdict + keep control-plane out of PRs ([`b7ce30d`](https://github.com/Elysium-Labs-EU/argus/commit/b7ce30dba92c0bf956b3dfa35822ea8255d22a71))
- Log tokens + run summary, add argus stats ([`7c08c20`](https://github.com/Elysium-Labs-EU/argus/commit/7c08c20f5bbc554f1e62e616a673ae453e4181fd))
- Forge abstraction (GitHub+Codeberg), issue briefs, richer PR body ([`d8e8b3a`](https://github.com/Elysium-Labs-EU/argus/commit/d8e8b3a069561638f1f2163ada1d4141426c07c1))
- Always-review behavior-critical paths; report spawn orphans ([`e7bd8f0`](https://github.com/Elysium-Labs-EU/argus/commit/e7bd8f0b102baca2eccb6421334f0a4435490c88))
- --attach watches already-running workers, no spawn ([`410295d`](https://github.com/Elysium-Labs-EU/argus/commit/410295d017d473c6459d8280e05aadc85e7a889b))
- Add GitLab implementation ([`db8f7e4`](https://github.com/Elysium-Labs-EU/argus/commit/db8f7e455d505fb61e1a63ba76f8570c6c79e6ca))
- Add issue-source client for Jira Cloud ([`8270624`](https://github.com/Elysium-Labs-EU/argus/commit/8270624a76c59165e955d654cf39d17c1576fd48))
- Pluggable worker-runtime isolation adapter (docker/podman/none) ([`6faa024`](https://github.com/Elysium-Labs-EU/argus/commit/6faa02459cf23bd8082254625f0999d2b245015e))
- Wire Jira issues into --jira-issues ([`561d0c0`](https://github.com/Elysium-Labs-EU/argus/commit/561d0c0258c9166bf049ae4e361b5267deb54b89))


### Maintenance
- Migrate repo identity from Codeberg to GitHub ([`175f6cf`](https://github.com/Elysium-Labs-EU/argus/commit/175f6cfc822ecf0b62537195fff113d5e170d771))
- Harden worker environment (cred proxy + forge/jira token scrub) ([`a98fadf`](https://github.com/Elysium-Labs-EU/argus/commit/a98fadffdb722f2c3f602e8b14436864fc834d31))
- Add LICENSE, generalize forge docs, default ast-grep/gitnexus tooling ([`6fbcc3d`](https://github.com/Elysium-Labs-EU/argus/commit/6fbcc3dd157295fb8dfa6819aa3835be2ff1b100))


### Refactoring
- Extract foldIssueSources to keep spawnWorkers under the CRAP gate ([`d86d774`](https://github.com/Elysium-Labs-EU/argus/commit/d86d7749378e054a078a5450a08e971786243eb6))


### Testing
- Raise coverage to 80% and lock the gate at 75% ([`d6a90ad`](https://github.com/Elysium-Labs-EU/argus/commit/d6a90adc9bf95dc32bfb723eacb468dbebb77bde))

