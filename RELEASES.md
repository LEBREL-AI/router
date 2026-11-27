# Releases and measurements

Every release of the router that serves traffic is listed here with what a third party needs to
check that the running machine is this code:

| Version | Git tag | Image digest | dstack compose hash | Receipt key id | Since |
|---|---|---|---|---|---|
| 1.4.5 (attested endpoint, instances B2 and A2, app `40b35a05…`, Phala KMS 2026-11) | `v1.4.5` | `sha256:95b83fdb8f044b6953a0003f478fd6b8262727e7861bb2c5718408d841b7a053` (linux/amd64, Go 1.26, BuildKit v0.27.1, OCI media types; two builds identical; `deploy/dstack/docker-compose.attested.yml`, reached through the relay described below; a call to a host outlives the customer who asked: an answer nobody waited for is charged as the host counted it) | `caf66b28767d9752de3c7eb2f0b37838d2d74e862f1a81bcffc65a49c0eda3be` (both instances) | receipt key id `2350313ddfae740898cfbeb1673c6342033336b976140fab4958307476f6ebe9`, public key `c1a85c91168537ec65638916d11f9c6cf59e75b9ef94db4c368f9c52c2ff6797` (the same on both instances: one app, one compose) | 27 Nov 2026 04:24 UTC (instance A2, updated at 03:49 UTC while it stood by; instance B2 from 04:28 UTC, the one that serves) |
| 1.4.4 (attested endpoint, instances B2 and A2, app `40b35a05…`, Phala KMS 2026-11) | `v1.4.4` | `sha256:af9f8dd34b40346172945653ec711466545068bb939a60240b0239843cd6ec79` (linux/amd64, Go 1.26, BuildKit v0.27.1, OCI media types; two builds identical; `deploy/dstack/docker-compose.attested.yml`, reached through the relay described below; usage past a request's envelope is charged as the host counted it, up to the amount held) | `66d8089dad6c287c02c493e29b454c8323a815550392a6ac03a56610f7682c44` (both instances) | receipt key id `8e8352f44a86abbaa9b3ba37d11503a3b48530b419928a6f04b746a2649f8dee`, public key `34626db515dcc553e1c1e0111591b8d4edca8fecdcc460c818798ac1099e231e` (the same on both instances: one app, one compose) | 27 Nov 2026 03:24 UTC to 04:26 UTC (instance A2 from 03:06 to 03:47 UTC; instance B2 from 03:28 to 04:26 UTC) |
| 1.4.3 (attested endpoint, instances B2 and A2, app `40b35a05…`, Phala KMS 2026-11) | `v1.4.3` | `sha256:b8fdc708966274b76f3c35f73cdaf7c5cc0a0fb534810ce657bde9e6b48eed69` (linux/amd64, Go 1.26, BuildKit v0.27.1, OCI media types; two builds identical; `deploy/dstack/docker-compose.attested.yml`, reached through the relay described below; what a request carries upstream: the output cap under both of its names, listed fields only, text and images only, a PDF read in a process of its own, a key's turns taken before a translated request is read; see the README) | `199f416483325b5ee1b504edbe91b18d08caf8e4c1565f5009dee30e8b4e45ec` (both instances) | receipt key id `a8872561d30baef854b7b08af6e7e3c4a636dbe6e1d0ef372e493b27b69b5194`, public key `34ae9a78df4ec211ebd6aa4bb9844a33ed392773094977447058e589b14f80fe` (the same on both instances: one app, one compose) | 27 Nov 2026 02:26 UTC to 03:26 UTC (instance A2 from 01:59 to 03:04 UTC; instance B2 from 02:30 to 03:26 UTC) |
| 1.4.2 (attested endpoint, instances B2 and A2, app `40b35a05…`, Phala KMS 2026-11) | `v1.4.2` | `sha256:68095f3972e77610443785557e03a6c6388facfa9d909f70a0d6a268a09d8d9b` (linux/amd64, Go 1.26, BuildKit v0.27.1, OCI media types; two builds identical; `deploy/dstack/docker-compose.attested.yml`, reached through the relay described below; a request is held for all of it: every field's name and value counts toward the prompt estimate, and only a picture's own data in a message's content is left out; reported by Lydicius) | `3c01d008887bc9799e6992bee04d30e8a2ccc757eef7cefd2bc28ac97982d8af` (both instances) | receipt key id `d929441c23ec3a27677c24a60ed471192caaf87c2d825a9962c9f2ad606ff8eb`, public key `061d36a16c131db17419144ef455430fdc8cebb5b3f9fa60a2f0dcb233521503` (the same on both instances: one app, one compose) | 27 Nov 2026 00:26 UTC to 02:28 UTC (instance A2 from 00:14 to 01:57 UTC; instance B2 from 00:29 to 02:28 UTC) |
| 1.4.1 (attested endpoint, instances B2 and A2, app `40b35a05…`, Phala KMS 2026-11) | `v1.4.1` | `sha256:9eff8ce89042e4287c908cc27e213b6ef3a325423e880b3da19c72081e94c91e` (linux/amd64, Go 1.26, BuildKit v0.27.1, OCI media types; two builds identical; `deploy/dstack/docker-compose.attested.yml`, reached through the relay described below; two instances behind the name, one that serves and one that stands by, each with its own certificate; Alibaba Cloud Model Studio is given five minutes to start answering a request of 512 KiB or more) | `e394ed2dfdf2337e960c2c34e87028950084140335d8ca5794dc638ce7097612` (both instances) | receipt key id `9246adfb6e899800d38c87aa3ed46f6af0a5cbe40157d48eb837e35829faf0d3`, public key `6d14b06daa514d8ab7e4800fa341a7af41a584cb7a0cc088e5935f051b79491a` (the same on both instances: one app, one compose) | 23 Nov 2026 02:23 UTC to 27 Nov 2026 00:27 UTC (instance B2; instance A2 from 23 Nov 02:44 UTC to 27 Nov 00:14 UTC) |
| 1.4.0 (attested endpoint, instance A2, app `40b35a05…`, Phala KMS 2026-11) | `v1.4.0` | `sha256:10a8ea4537ecc531a3121a1e58bf0efb510d7eebb13e6577b816e968581cf25b` (linux/amd64, Go 1.26, BuildKit v0.27.1, OCI media types; two builds identical; `deploy/dstack/docker-compose.attested.yml`, reached through the relay described below; images and documents for Claude Code and Codex, image inputs on the models that read them, the edition's own output limit) | `a3adaa9e2bb1c45bb3f4eea3a6fa1945265b50646552833db5fbc3ab30a5d69d` | receipt key id `5d10b710887827859376d1c36a07964e466ae1835c39cc3bad9f2d8aa5bcb448`, public key `abdf46003beee2f595f44371c5659c83799f5c8543af609b232c38450586bd8c` | 16 Nov 2026 20:17 UTC to 23 Nov 2026 02:43 UTC |
| 1.3.9 (attested endpoint, instance A2, app `40b35a05…`, Phala KMS 2026-11) | `v1.3.9` | `sha256:bd9994440edf81ec303008d0c33137e2ba64da0d59fcefcfafcc2c6209d9818b` (linux/amd64, Go 1.26, BuildKit v0.27.1, OCI media types; two builds identical; `deploy/dstack/docker-compose.attested.yml`, reached through the relay described below; Qwen4 Max, and a route whose providers all refuse for the moment is asked again before the customer is told) | `4830ccd86b255d53227ddfa49ea94875e46c711d355ca80564809a25936952df` | receipt key id `ccaf046e6110390973a8570d80a5d551ce47425bb1c33dbf087bed2f285be532`, public key `2b4121e3f702dfac3f842c6fd2a24341af48ed44ec8a37458ac90c0c94fa6e7d` | 16 Nov 2026 03:23 UTC |
| 1.3.8 (attested endpoint, instance A2, app `40b35a05…`, Phala KMS 2026-11) | `v1.3.8` | `sha256:de1cfaa9fe6218af1748bf5de38289b245f92099aba096e574c1f71cd7a40540` (linux/amd64, Go 1.26, BuildKit v0.27.1, OCI media types; two builds identical; `deploy/dstack/docker-compose.attested.yml`, reached through the relay described below) | `b7a8b290d061e36d6f92ee461acf094d87bafdcc90e65abd6e3f4d5d72012ded` | receipt key id `f97cc982d215a317660f9fd3eeb0b51a9ff3dbef2f76ccc5ceb8303b4623bbfa`, public key `135db754b2792320df1e73d2f4b742bf079f3cb4ecff0b2f48474832653e7763` | 11 Nov 2026 02:09 UTC |
| 1.3.7 (attested endpoint, instance A2, app `40b35a05…`, Phala KMS 2026-11) | `v1.3.7` | `sha256:ad3f533f0b7fd67b8df9cbf407a9cfb54c90f10c67fa8bd9a01c458bec52bd35` (`deploy/dstack/docker-compose.attested.yml`: TLS ends in the router, Let's Encrypt in-process, attested.lebrel.ai; reached through the relay described below) | `2bcd6faeb21e6c6edb805f923107bca8f3a210a04049361ecce355d2f9316465` | receipt key id `ecfe903e9da3fcee15ee0758ce5d716ce77617d2c0d5840c1330e518d372297c`, public key `59a65997e76163c6e97bed374cbfde543958899e1623c24c7e4ff1166b994342` | 10 Nov 2026 21:19 UTC to 11 Nov 2026 02:08 UTC |
| 1.3.7 (attested endpoint, instance A) | `v1.3.7` | `sha256:ad3f533f0b7fd67b8df9cbf407a9cfb54c90f10c67fa8bd9a01c458bec52bd35` (`deploy/dstack/docker-compose.attested.yml`: TLS ends in the router, Let's Encrypt in-process, attested.lebrel.ai and router-tee.lebrel.ai; no dstack-ingress) | `15bd7be7b090da7b83859cfbe8f3277cf05cc67c1cf249486801159222939e54` | receipt key id `721568bba8eb1424b02aef6134572c6f928deec3f967e42dc49633a3ca39f159`, public key `5b10c282dba4d68171cc2c629a8e277654cf0f4819e944e6dc3cd46faaa175a5` | 6 Nov 2026 08:20 UTC |
| 1.3.7 | `v1.3.7` | `sha256:ad3f533f0b7fd67b8df9cbf407a9cfb54c90f10c67fa8bd9a01c458bec52bd35` (linux/amd64, Go 1.26, BuildKit v0.27.1, OCI media types; two builds identical) | `2ba084cd6a4bb044e6a03a4d2a7902bb7ae7b88a52e4ff707fd9681a63b7b663` (gateway compose, replica B) | receipt key id `95f8e35094b2aea22ceb3ff27a123871ef7092b2bdcf216e54527eaf9b649a82`, public key `dca6c014ee3f32c82f4a1fec440e66effd130de5b65808a7e54ec980b018e3b3` | 6 Nov 2026 08:20 UTC |
| 1.3.6 (custom domain, instance A) | `v1.3.6` | `sha256:d3725f05e6017caee09adeb431c2b2e3d70b010c92cfc99f8bb4c55bd26c8b54` + dstack-ingress 2.2 (`deploy/dstack/docker-compose.custom-domain.yml`, router-tee.lebrel.ai) | `aa4f6387441feb40048718db273a58ab4c0457d8832580fbaff0470d9f44d835` | receipt key id `5ea853292f89ef2fa181fbff6c978b979c855c9a5ecbe2201bfb4d0478847872`, public key `832c4891654af084f2366d7156bc2bb3141fc0270920dbfd160d4d317612bec2` (an app key derived inside the VM); the request-encryption key is born at random inside the VM per boot and per UTC day and named only by the attestation | 2026-11-06 |
| 1.3.6 | `v1.3.6` | `sha256:d3725f05e6017caee09adeb431c2b2e3d70b010c92cfc99f8bb4c55bd26c8b54` (linux/amd64, Go 1.26, BuildKit v0.27.1, OCI media types; two builds identical) | `380bf44b9121927380ff25aa63ae0b787dfdce171de69e830ae4ccf4878205c5` (gateway compose, replica B) | receipt key id `1376e5c9a109529b7d8c9629ea315e989847849f73a7753597feac6860ca5fe3`, public key `147f868cadcf9e37f89dfe6d8f1e4680298a6c78963f0bc24f14cf511bbd7b16` (an app key derived inside the VM); encryption key random per boot and per day, see the attestation | 2026-11-06 |
| 1.3.5 (custom domain, instance A) | `v1.3.5` | `sha256:b31316b3d211ded8cf320a87f8c02334f787cd0b9113560274e98eeb8800345a` + dstack-ingress 2.2 (`deploy/dstack/docker-compose.custom-domain.yml`, router-tee.lebrel.ai) | `675613867a396b1a453c8d6315b48b343fc57aa41ee4693a701075bb8a65b58a` | receipt key id `806321d5e59607ff6bb488b2bd5a52efe5d30cedafa6968bd55dfbe328fcc63f`, public key `658837f206f46d81e5a2b519cbae60c149916da933a1197523fe11f74bf4b8b6` (an app key derived inside the VM); the request-encryption key is born at random inside the VM per boot and per UTC day and named only by the attestation | 2026-11-05 |
| 1.3.5 | `v1.3.5` | `sha256:b31316b3d211ded8cf320a87f8c02334f787cd0b9113560274e98eeb8800345a` (linux/amd64, Go 1.26, BuildKit v0.27.1, OCI media types; two builds identical) | `dabc6a3469d4d9c773e273479397906b4bd1737f437b107a90d1a3ef6df85383` (gateway compose, replica B) | receipt key id `481563de439a7c1bb9baeeabfef91f921fc244d35aeecb5e81a797d4eadafdd0`, public key `30da77f128fc2125de8f5a90e6ae30f4fc36931e293e01f87d41b5fa8a198d81` (an app key derived inside the VM); encryption key random per boot and per day, see the attestation | 2026-11-05 |
| 1.3.4 (custom domain, instance A) | `v1.3.4` | `sha256:2c7a7fcc8714013d72bc44eccfad3e2150d7303f7194c6e8eb3a3e2eb77f2701` + dstack-ingress 2.2 (`deploy/dstack/docker-compose.custom-domain.yml`, router-tee.lebrel.ai) | `031f72e1ebce5f36acf91c8c1f552b50d6e9825d45395d048832370c2348a556` | receipt key id `74a62e2583a472ec262c31579e9f39a0b18bbe0a9151141cb66afb53686bb243`, public key `bbb761ab3a933392d74cbaea44477b24f506db9e3f0a4018de53b69acdeda87f` (an app key derived inside the VM); the request-encryption key is born at random inside the VM per boot and per UTC day and named only by the attestation; replaced by 1.3.5 the same day | 2026-11-05 |
| 1.3.4 | `v1.3.4` | `sha256:2c7a7fcc8714013d72bc44eccfad3e2150d7303f7194c6e8eb3a3e2eb77f2701` (linux/amd64, Go 1.26, BuildKit v0.27.1, OCI media types; two builds identical) | `35b9818cf8b7fe2de2c93a1ab2ba000b571eaa8d4060e7f48d437d7c45dfba85` (gateway compose, replica B) | receipt key id `0e4cce3ed12ffa64001b6c2dcb2409630dd1078a1e4e12fe7bc38483df1f2f74`, public key `036127e360cf5d7f53d8c9ede74daecca5398dd27338feef3bd5d86e7996169f` (an app key derived inside the VM); encryption key random per boot and per day, see the attestation; replaced by 1.3.5 the same day | 2026-11-05 |
| 1.3.3 (custom domain, instance A) | `v1.3.3` | `sha256:6c46fb6a13c8ae323eac6b337a6ec17667c3311cc2e10415994a17b308ebe13e` + dstack-ingress 2.2 (`deploy/dstack/docker-compose.custom-domain.yml`, router-tee.lebrel.ai) | `a889ba12c4c405d66ee12a3ceb3ee646fc084fc2d02f53ce15ae01dcf138218e` | receipt key id `317187d2a961ea51719eaf395bb8b203ebc728f57a6a68a1c145e84b6be68e9d`, public key `d9b50f57d71d97fa6af3525bc5f9ec9f5b320c35d088a4d3019009f55a17eb3e` (an app key derived inside the VM); the request-encryption key is born at random inside the VM per boot and per UTC day and named only by the attestation; replaced by 1.3.4 the same day | 2026-11-05 |
| 1.3.3 | `v1.3.3` | `sha256:6c46fb6a13c8ae323eac6b337a6ec17667c3311cc2e10415994a17b308ebe13e` (linux/amd64, Go 1.26, BuildKit v0.27.1, OCI media types; two builds identical) | `4601bebe4c37238a6e9a2bf957aef66f935917175afd896c8f397f341a20660b` (gateway compose, replica B) | receipt key id `d513d290f575baff49a41087e744ee947cf728a813dbea773616ba190ac6ae51`, public key `37b48947fdb4d820173dcc1d1133e9a060f1630c0cc14ce0a661d1963870ea28` (an app key derived inside the VM); encryption key random per boot and per day, see the attestation; replaced by 1.3.4 the same day | 2026-11-05 |
| 1.3.2 (custom domain, instance A) | `v1.3.2` | `sha256:0c59d65c11d366a1fd8f61e49158a92c21d0baa79219b6ffda907bcc92c37179` + dstack-ingress 2.2 (`deploy/dstack/docker-compose.custom-domain.yml`, router-tee.lebrel.ai) | `e17840197f21c6eb27a55963751b67a8d2e3f3192074512919c69814b478a0f9` | receipt key id `a3f2ea0a6f8abedc10bacadc1bb0029b0795309cbf796c53b4c830e6b69fced4`, public key `f679f933d3853339767d14f5982a03ed383283b236ca1f13f6f20e7848cd1df3` (an app key derived inside the VM); the request-encryption key is born at random inside the VM per boot and per UTC day and named only by the attestation; replaced by 1.3.3 the next day | 2026-11-04 |
| 1.3.2 | `v1.3.2` | `sha256:0c59d65c11d366a1fd8f61e49158a92c21d0baa79219b6ffda907bcc92c37179` (linux/amd64, Go 1.26, BuildKit v0.27.1, OCI media types; two builds identical) | `bd625f569b5fbe2cd9087bbbb862adb6caf572342868abbd6c81370c5bad51f0` (gateway compose, replica B) | receipt key id `937c1b37e23516003783af5754480f6c0aa6247fd161cdd259e9c9cbf7f40c31`, public key `3b4f987738dbbf99dbaea6ff682d7d61a6c9245d65a52a0efcc636e5ac2cf5f4` (an app key derived inside the VM); encryption key random per boot and per day, see the attestation; replaced by 1.3.3 the next day | 2026-11-04 |
| 1.3.1 (custom domain, instance A) | `v1.3.1` | `sha256:9f8e09106cc27aca032c6563fe9025867f492061221428c501ae29c52548d75e` + dstack-ingress 2.2 (`deploy/dstack/docker-compose.custom-domain.yml`, router-tee.lebrel.ai) | `195352a13fe7c372a54355e898108bbf7434fb4066948d9a23204c7a7fa47ba7` | receipt key id `dc84f9b3976650ee72123bec501c57bbebc819d23cb680132afe07e1c07817d2`, public key `d2cd0eab4adf24c1b7c0921b30d5fe2ee82c4ffc3ae22a4f1e8252609ac10928` (derived inside, compose in the path); the request-encryption key is derived per UTC day and named by the day's attestation, so it is not fixed here; replaced by 1.3.2 the same day | 2026-11-04 |
| 1.3.1 | `v1.3.1` | `sha256:9f8e09106cc27aca032c6563fe9025867f492061221428c501ae29c52548d75e` (linux/amd64, Go 1.26, BuildKit v0.27.1, OCI media types; two builds identical) | `d91af75112c9dce5272de46fb013ad15177877297302f9aa69fce550625b4eb7` (gateway compose, replica B) | receipt key id `f711f517a629c650a2d1d6ad133e7a93553f11a15130bc0bbc4183e1e0ec1597`, public key `6daed4052008a1e9b48136495b9124038a7ca69d2bd2804ed35fe835671374d3` (derived inside, compose in the path); encryption key per UTC day, see the attestation; replaced by 1.3.2 the same day | 2026-11-04 |
| 1.3.0 (custom domain, instance A) | `v1.3.0` | `sha256:88f572f4830c4df1fbf29e9c36c4375760a803fcff9c3ba1499662a712a5e3e2` + dstack-ingress 2.2 (`deploy/dstack/docker-compose.custom-domain.yml`, router-tee.lebrel.ai) | `0a9e1b732dc2cffdcd4e65a3b11372da4aea0c61a470bdcf256eaae0aaf293a3` | receipt key id `fef2039efa223f3543068af08c854e9f755c559da5254b7ac6e6f35e3bdde138`, public key `ab1ef721a6aa2ae2bc093cebe92385af2e3834ac976ad852dab576b1ce825f93`; encryption key id `b14f0b47bcdfd29414518257aa11df2db23de2c2e56ea2a76ae3f2b9682245b3`, X25519 public key `a2bc21eb45e0cd177eda827f9748f0a2538a328fa0053ce0b093dd149431511d` (both derived inside, compose in the path; replaced by 1.3.1 the same day) | 2026-11-04 |
| 1.3.0 | `v1.3.0` | `sha256:88f572f4830c4df1fbf29e9c36c4375760a803fcff9c3ba1499662a712a5e3e2` (linux/amd64, Go 1.26, BuildKit v0.27.1, OCI media types) | `8a1a1bd144c5bd23f1e9dd9244b1fd0ce64f956804fe70cd42420500bfe9ab02` (gateway compose, replica B) | receipt key id `7f9adc456058e8f5505bc065d161053010c958ac496115859c7e0c7d53b976f1`, public key `012bb8944ffa04104b1bf4a23578bf670ee9554e7e84f49319f9da4ac015b8d2`; encryption key id `cb2dead9fad835a95677dac8740ddba5ecb225fe97d567d6ccd00d723a8f4809`, X25519 public key `91193f70bf5ee51cdd9ca8ace141f616d98db21a647e1a9fa1baa5914c48f212` (both derived inside, compose in the path; replaced by 1.3.1 the same day) | 2026-11-04 |
| 1.2.0 (custom domain, instance A) | `v1.2.0` | `sha256:de4aaa36efa8750a85ce3a90331e0259bcf6c2bfd04e5232152452a147ac58a6` + dstack-ingress 2.2 (`deploy/dstack/docker-compose.custom-domain.yml`, router-tee.lebrel.ai) | `0b5563830f207a2f2ff03042505985d7ecd96935f2f7a1f074e84b493c8cba17` | `75b981a701b150c565d05500191dbe6a83dfa0c1bcabe8752234f41067a05204`, public key `161dc88748c3d05ee654bd56285b71115c1c5dfabc5ff9967f6ffbc21287588d` (derived inside, compose in the path) | 2026-11-04 |
| 1.2.0 | `v1.2.0` | `sha256:de4aaa36efa8750a85ce3a90331e0259bcf6c2bfd04e5232152452a147ac58a6` (linux/amd64, BuildKit v0.27.1, OCI media types) | `a28254ec21ff869593b25cf2d749c15c82563be1d676af4087cea5e9603bf9bf` | `24850d5e65638769…` (derived inside, compose in the path) | 2026-11-04 |
| 1.1.0 | `v1.1.0` | `sha256:31eb44606b599012143e85ea630e80b90095b080bc65442f8f6ba7e88e0dfd5d` (linux/amd64) | `3ba7b773f9c3b844ec4f4641c7384437a383051ed8328742c1595aac6a372c58` | `19bd924d…` (derived inside, app-wide path) | 2026-11-04 |
| 1.0.2 | `v1.0.2` | `sha256:66962f2e136059cbaefed0b2ef7b7edddf756c4b4c035a6f9b0ae466f9157fc5` (linux/amd64) | `50d65ee7c8b31f316a3dbbf5060f884df6ef522dd2a4f6df5c953799e4b5c03c` | handed in (`RECEIPT_SIGNING_SEED`) | 2026-11-04 |
| 1.0.1 | `v1.0.1` | `sha256:1b0079066e90ca902c0e11db6b139dc7e40ea0aa00c9eb796ebc6741da23e574` | `8b33778884c7441d04adc00bcd19b5398084bcdf72caec5097d553af0a934d4e` | handed in; never served (restarted) | 2026-11-04 |
| 1.0.0 | `v1.0.0` | `sha256:67566c5629e9541d9188feee2955034b6bb16def2ecf9d1a6585317363c9c465` | `38b091826b3ce68e832d61e95282b358104717e1825b778c12c3d231b1980c24` | handed in; never served (restarted) | 2026-11-04 |

How a release is made: tag the commit; run `scripts/reproducible-build.sh <tag>` on a clean tree
(two clean builds with the pinned BuildKit named in the script, same manifest digest and same
compression-independent identity, or the release stops); push that image with `scripts/push-image.sh <tag>` (same builder, OCI media
types: the registry digest must equal the local one) and pin its digest in the compose; deploy the compose with dstack; record
here the compose hash dstack reports, the image digest, and the receipt key id the deployment
derives.

How anyone checks a running router: `scripts/verify-attestation.py <router> --compose-hash <row>
--app-id <the row's app> --key-id <row> --reference deploy/dstack/reference-measurements/<the app's file>
--compose-file deploy/dstack/<the row's compose>`. The machine that serves today (first row):
`scripts/verify-attestation.py https://attested.lebrel.ai --compose-hash e394ed2dfdf2337e960c2c34e87028950084140335d8ca5794dc638ce7097612
--app-id 40b35a054d2eff752b1b5c3e4dee7253ab941f68 --key-id 9246adfb6e899800d38c87aa3ed46f6af0a5cbe40157d48eb837e35829faf0d3
--reference deploy/dstack/reference-measurements/dstack-0.6.1-phala-kms-2026-11.json --compose-file deploy/dstack/docker-compose.attested.yml`.
The rows below it belong to the earlier app (`4d72eae647c67ad42c972d24175d7c32ce9c871a`, reference file `dstack-0.6.1.json`), which no longer runs the router.
It asks the machine for a quote with a fresh nonce, verifies Intel's chain (dcap-qvl), recomputes
every runtime event of the log and replays the log into the RTMRs, reads the compose hash, app id
and key provider from the verified log, and compares them with the row and the arguments.

What the key proves, exactly. The receipt-signing key is derived by the VM's KMS (Phala's, named
`kms` in the measured key-provider event) for this app id, with the compose hash in the
derivation path: a receipt signed with the key of a row was signed by a VM running that app under
that measured compose. The trust anchors are Intel (the quote), the KMS operator (it releases keys
only to measured VMs of the app) and the app's owner, who alone can register new composes for the
app: a later row has a different key, and the owner, through a new compose, could derive the
keys of earlier rows (the KMS derives from the app's root key and the path): a receipt is
attributable to a row only as far as the owner is trusted not to do that, which is why the owner
is named here as an anchor. Every container of a measured compose can reach the guest
agent's socket, so the claim covers the whole compose, not the router container alone. The hash
covers dstack's whole app manifest, not only the compose file: the deployment flags (public logs
off, public system info off, public attestation data on, KMS key provider) are part of it, so a
change of any of them is a new row.

Rows 1.0.0–1.0.2 were deployed with the seed handed in as an encrypted environment variable
(`RECEIPT_SIGNING_SEED`), which a human held: their receipts prove the measured compose only as
far as that human is trusted. From 1.1.0 the seed is derived inside; from 1.2.0 the derivation
includes the compose hash and an explicit seed is refused inside a VM in production.

From 1.3.1 the quote's report data binds the receipt key, the encryption key and the verifier's
nonce each behind its length (v3); fresh quotes wait their turn in a short line instead of being
counted per address; and a client may put a salt first inside its encrypted body
(`lebrel_receipt_salt`), which the router cuts out and uses to salt the receipt's digests
(`hash_salt_scheme: client-salt-v1`), so a receipt's prompt and answer hashes are checkable by the
two ends only.

From 1.3.2 the request-encryption key is born at random inside the VM, per boot and per UTC day
(the previous day's kept one more day), and is never derived from the VM's key service: that
service derives keys per app, so a key taken from it could be derived again by any compose the
app's owner registers, whereas a key born in the VM's memory exists nowhere else and the quote
binds it. The receipt-signing key stays an app key derived inside the VM (the same for every
compose registered under the app, which is what makes old receipts verifiable): a route manifest
says with `attested: true` when its signer runs inside an attested VM, and a data note never claims
more. The API key is checked before an encrypted body is read, an encapsulation is remembered under
its account (4,096 live entries each, 50 hours, longer than a key's life) and forgotten again when
the request is refused before credit is reserved.

**Incident, 6 Nov 2026.** Phala Cloud reported that an exploit on its platform earlier that day may have
leaked the root key of the app these two instances belong to. The receipt keys of the rows 1.1.0 to 1.3.6
are therefore not to be trusted for anything signed after 6 Nov 2026 02:00 UTC; the control and edge
secrets were rotated the same morning. The 1.3.7 rows above were deployed in place on the same app, so
their keys are still derived under that app: the move to Phala's replacement KMS (new instances, new
app id) follows and gets its own rows.

**The move, 10 Nov 2026 21:19 UTC.** The router that serves `attested.lebrel.ai` is instance A2 of a new app
(`40b35a054d2eff752b1b5c3e4dee7253ab941f68`) created under Phala's replacement KMS ("Phala KMS 2026-11"): its
receipt key, the key of the name's certificate and its sealed secrets were never under the earlier KMS (first
row; same image, same compose file, same OS measurements; the manifest's flags are public logs off, public
system info off, public attestation data on). Every row of the earlier app, 1.1.0 to 1.3.7, is retired: a
receipt signed with one of their keys after 6 Nov 2026 02:00 UTC carries the incident's doubt, and none is
signed after 10 Nov 2026 21:17 UTC.

The gateway of the new KMS's zone does not connect custom names to its apps yet, so the name is still
connected to the earlier app, whose two instances now run only a relay
(`deploy/dstack/docker-compose.relay.yml`, the official haproxy image pinned by digest, its whole
configuration in that file; manifest hashes until 23 Nov 2026 `fe259a1cea4cc683125bbfc7d56e3194385a5cb2eb4bb2dc526912a5d4ec6955`
on prod9, then the one in the name's path, and `c2f3e534ce5ef037874a939b147960bf7587e47ea1e02641b422ccc51ad28775` on
prod5; the ones since are in the paragraph on the two instances below). The relay passes each connection, unread, inside its own verified TLS to the new app's gateway
hostname, which hands it to the router's TLS listener: the client's TLS session ends in the router of the new
app, and neither gateway nor the relay holds a key of it. What the relay does not change is who decides which
machine receives the name's connections: the earlier zone's gateway, which knows the earlier app by
certificates of the earlier KMS. Until the name is connected to the new app directly, the issuance of its
certificate is to be pinned in DNS to the router's own ACME account (a CAA record), which a later row
records. The Python client verifies this machine from 0.5.3.

**Two instances, 23 Nov 2026.** From 1.4.1 the name is served by two instances of the app: B2 on prod5, which
serves since 23 Nov 2026 02:23 UTC, and A2 on prod9, which stands by since 02:48 UTC (it ran 1.4.0 until 02:43 UTC
and was the only instance until 02:23 UTC). Both run the first row's compose, so both sign receipts with the first row's key, and a receipt
of an answer one of them gave is handed over by the other. Each holds its own certificate for the name, obtained
with its own ACME account (B2: `https://acme-v02.api.letsencrypt.org/acme/acct/4031246402`; A2:
`https://acme-v02.api.letsencrypt.org/acme/acct/3982221542`), and its own request-encryption key, born in its
memory: an attestation names the instance it was made for, and a client encrypts to the instance it verified.
The relay's two instances carry one configuration since that day (`deploy/dstack/docker-compose.relay.yml`, written
by `deploy/dstack/relay-config.py`; manifest hashes `86e3b6685374bc4449ef24cff3eace9f517e35e762532942aa55c5d8d760c3bb`
on prod9 and `5433e3c3d53f7a8ef467d2275b182795828c62eb552bf68bb77524f07fb5d3a6` on prod5, the one in the name's
path since the name's DNS record points there): a connection goes to the first router whose check says it may
take customers, B2 and then A2, as the file's header tells. The Python client verifies these machines from 0.5.6.
Release 1.4.2 was rolled out through that order on 27 Nov 2026: A2 was updated while it stood by, the name's
customers moved to it at 00:26 UTC, B2 was updated empty and serves again since 00:29 UTC. The Python client
verifies release 1.4.2 from 0.5.7.
Release 1.4.3 followed the same order the same night: customers on A2 from 02:26 UTC, B2 serving again since 02:30 UTC. Release 1.4.4 likewise: customers on A2 from 03:24 UTC, B2 serving again since 03:28 UTC. Release 1.4.5 likewise: customers on A2 from 04:24 UTC, B2 serving again since 04:28 UTC. The Python client verifies 1.4.3 from 0.5.7, 1.4.4 from 0.5.8 and 1.4.5 from 0.5.9.

`router-tee.lebrel.ai` is not served any more: `attested.lebrel.ai` is the one name, for clients and for the
platform's edge alike.

From 1.3.3 an encrypted request also carries the moment it was sent, inside the ciphertext
(`"lebrel_sent_at":<unix seconds>,` right after the salt, or first; required): a request older or
newer than five minutes is refused, so an encapsulation is remembered for eleven minutes rather than
for its key's life, the table holds minutes of traffic (one million entries, 65,536 per account, far
above any customer's rate), every refusal before credit is reserved forgets its entry, and the key's
and the account's concurrency slots are taken before the encrypted body is read, as on the plaintext
path. A route's attestation is quoted again, once, when the encryption key rotates while the
machine quotes.

From 1.3.4 bytes in flight are bounded as well (one unit per MiB of body: 128 per key or the
key's own concurrency when higher, 256 per account or the account's, 1,024 for the machine), the
per-account share of the replay table is 262,144 entries, the shared attestation document is
renewed outside the quote line (one renewal at a time, the others waiting for it ten seconds at
most, so a crowd asking for fresh quotes starves nobody and a slow agent lines nobody up), and a
document is not cached if the key rotated since its quote.

Besides `deploy/dstack/docker-compose.attested.yml` (the first row) and `docker-compose.relay.yml`, two
earlier compose files are kept: `deploy/dstack/docker-compose.yml` (the machine reached through the
dstack gateway hostname; its TLS terminates at Phala's gateway, outside the quote) and
`deploy/dstack/docker-compose.custom-domain.yml` (the same router with dstack-ingress beside it,
terminating TLS for router-tee.lebrel.ai inside the VM). Each has its own compose hash; the ledger
names which one a row was deployed from; the replica on prod5 keeps the gateway compose (one ingress
per domain), so the two instances sign with different keys. A compose-only update (`phala deploy
--cvm-id … -c <compose>` without `-e`) keeps the instance's encrypted environment, verified on the
replica with a real compose change. Reference measurements of the dstack OS image, as
observed on the quotes (not an independent rebuild): `deploy/dstack/reference-measurements/`.
