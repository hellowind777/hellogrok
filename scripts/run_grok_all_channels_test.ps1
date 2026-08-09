param(
    [string[]]$Models = @(),
    [int]$TimeoutSeconds = 150,
    [int]$MaxTurns = 1,
    [string]$Prompt = "Reply with exactly: OK. Do not call tools.",
    [string]$SearchPrompt = "Call web_search exactly once to find the latest commit in the public xai-org/grok-build GitHub repository. After that tool result, do not call any tool again. Reply with the repository name, the first 7 characters of the current HEAD commit, and one source URL. Do not answer from memory.",
    [string]$SubagentSearchPrompt = "Call spawn_subagent exactly once with subagent_type general-purpose. Tell the child to use web_search now to find the latest commit in the public xai-org/grok-build GitHub repository and return the repository name, the first 7 characters of the current HEAD commit, and a source URL. Do not search in the parent. Wait for the child and return its result.",
    [string]$FetchPrompt = "Use the web_fetch tool, not web_search, to fetch https://example.com/ and reply with the page title.",
    [string]$ExpectedSubagentModel = "",
    [switch]$RequireWebSearch,
    [switch]$RequireSubagentSearch,
    [switch]$RequireWebFetch
)

$ErrorActionPreference = "Continue"
$grok = (Get-Command grok -ErrorAction Stop).Source
$realGrokHome = if ($env:GROK_HOME) { $env:GROK_HOME } else { Join-Path $HOME ".grok" }
$dataDir = Join-Path $env:LOCALAPPDATA "hellogrok"
$outDir = Join-Path $dataDir "channel-smoke"
$isolatedHomes = Join-Path $outDir "isolated-homes"
$proxyLog = Join-Path $dataDir "hellogrok.log"
New-Item -ItemType Directory -Force -Path $outDir, $isolatedHomes | Out-Null

if (@($RequireWebSearch, $RequireSubagentSearch, $RequireWebFetch).Where({ $_ }).Count -gt 1) {
    throw "RequireWebSearch, RequireSubagentSearch, and RequireWebFetch are mutually exclusive."
}
if ($ExpectedSubagentModel -and -not $RequireSubagentSearch) {
    throw "ExpectedSubagentModel requires RequireSubagentSearch."
}
if ($ExpectedSubagentModel -and $ExpectedSubagentModel -notmatch '^[A-Za-z0-9._/-]+$') {
    throw "ExpectedSubagentModel contains unsupported characters."
}

if ($Models.Count -eq 0) {
    $configPath = Join-Path $realGrokHome "config.toml"
    $discovered = [Collections.Generic.List[string]]::new()
    foreach ($line in Get-Content -LiteralPath $configPath) {
        if ($line -match '^\s*(?:base_url|api_base_url)\s*=\s*["'']http://127\.0\.0\.1:18787/c/([^"'']+)["'']') {
            $id = [Uri]::UnescapeDataString($Matches[1])
            if (-not $discovered.Contains($id)) {
                $discovered.Add($id)
            }
        }
    }
    $Models = $discovered.ToArray()
    if ($Models.Count -eq 0) {
        throw "No active hellogrok channel URLs found. Start the proxy or pass -Models explicitly."
    }
}

$searchProbe = $RequireWebSearch -or $RequireSubagentSearch
$effectivePrompt = if ($RequireSubagentSearch) { $SubagentSearchPrompt } elseif ($RequireWebSearch) { $SearchPrompt } elseif ($RequireWebFetch) { $FetchPrompt } else { $Prompt }
$outputFormat = if ($searchProbe -or $RequireWebFetch) { "streaming-messages-json" } else { "json" }

$results = foreach ($model in $Models) {
    $safeName = $model -replace '[^A-Za-z0-9._-]', '_'
    $stdout = Join-Path $outDir "$safeName.stdout.json"
    $stderr = Join-Path $outDir "$safeName.stderr.txt"
    $modelHome = Join-Path $isolatedHomes $safeName
    $leaderSocket = Join-Path $modelHome "leader.sock"
    $projectConfigDir = Join-Path $modelHome ".grok"
    New-Item -ItemType Directory -Force -Path $modelHome, $projectConfigDir | Out-Null
    # Keep the shared AnySearch source untouched while hiding it from this
    # isolated Grok process. Otherwise a model can narrate or invoke that skill
    # instead of exercising the hosted tool under test.
    $projectConfig = "[skills]`ndisabled = [`"anysearch`"]`n"
    [IO.File]::WriteAllText(
        (Join-Path $projectConfigDir "config.toml"),
        $projectConfig,
        [Text.UTF8Encoding]::new($false)
    )
    [IO.File]::WriteAllText($stdout, "", [Text.UTF8Encoding]::new($false))
    [IO.File]::WriteAllText($stderr, "", [Text.UTF8Encoding]::new($false))

    $started = Get-Date
    $logStartLine = if (Test-Path -LiteralPath $proxyLog) {
        @(Get-Content -LiteralPath $proxyLog).Count
    } else {
        0
    }
    $effectiveMaxTurns = if ($RequireSubagentSearch -and $MaxTurns -lt 4) { 4 } elseif (($RequireWebSearch -or $RequireWebFetch) -and $MaxTurns -lt 2) { 2 } else { $MaxTurns }
    $quotedPrompt = '"' + ($effectivePrompt -replace '"', '\"') + '"'
    $args = @(
        "-p", $quotedPrompt,
        "-m", $model,
        # Headless probes cannot answer permission prompts. The per-mode
        # allowlists below make bypassPermissions safe by excluding local
        # execution and filesystem tools before the model sees them.
        "--permission-mode", "bypassPermissions",
        "--max-turns", [string]$effectiveMaxTurns,
        "--output-format", $outputFormat,
        "--leader-socket", $leaderSocket
    )
    if (-not $RequireSubagentSearch) {
        $args += "--no-subagents"
    }
    if ($searchProbe -or $RequireWebFetch) {
        # Grok's CLI allowlist accepts the public Claude-compatible alias
        # "Agent" for spawn_subagent. Build automatically retains that tool's
        # task-status dependencies, so run_terminal_cmd must not be exposed.
        $allowedTool = if ($RequireSubagentSearch) { "Agent,web_search" } elseif ($RequireWebSearch) { "web_search" } else { "web_fetch" }
        $disallowedTools = if ($searchProbe) {
            "search_tool,use_tool,web_fetch,workflow,run_terminal_cmd"
        } else {
            "search_tool,use_tool,web_search,workflow,run_terminal_cmd"
        }
        $args += @(
            "--tools",
            $allowedTool,
            "--disallowed-tools",
            $disallowedTools
        )
    } else {
        # An empty --tools value means "no clamp" in Build. Allow only Web
        # Search and then disable Web Search so the effective set is empty.
        $args += @(
            "--tools", "web_search",
            "--disallowed-tools", "search_tool,use_tool,web_fetch,workflow,run_terminal_cmd",
            "--disable-web-search"
        )
    }

    # GROK_HOME keeps the real model configuration. An isolated HOME prevents
    # ~/.agents/skills (notably anysearch) from changing which search path wins.
    $savedHome = $env:HOME
    $savedUserProfile = $env:USERPROFILE
    $savedGrokHome = $env:GROK_HOME
    try {
        $env:HOME = $modelHome
        $env:USERPROFILE = $modelHome
        $env:GROK_HOME = $realGrokHome
        $process = Start-Process -FilePath $grok -ArgumentList $args `
            -WorkingDirectory $modelHome -WindowStyle Hidden `
            -RedirectStandardOutput $stdout -RedirectStandardError $stderr -PassThru
    } finally {
        $env:HOME = $savedHome
        $env:USERPROFILE = $savedUserProfile
        $env:GROK_HOME = $savedGrokHome
    }

    $finished = $process.WaitForExit($TimeoutSeconds * 1000)
    if (-not $finished) {
        Stop-Process -Id $process.Id -Force -ErrorAction SilentlyContinue
        $process.WaitForExit()
        $status = "timeout"
        $exitCode = -1
    } else {
        # Complete redirected-stream handling and refresh the process handle before
        # reading ExitCode. A timed WaitForExit alone can leave ExitCode unset in
        # PowerShell even though the child has already written valid output.
        $process.WaitForExit()
        $process.Refresh()
        $status = if ($process.ExitCode -eq 0) { "ok" } else { "failed" }
        $exitCode = $process.ExitCode
    }

    $newLogLines = if (Test-Path -LiteralPath $proxyLog) {
        @(Get-Content -LiteralPath $proxyLog | Select-Object -Skip $logStartLine)
    } else {
        @()
    }

    $proxyHit = $false
    $requestModel = ""
    $toolCount = 0
    $webSearch = 0
    $hostedWebSearch = 0
    $functionWebSearch = 0
    $xSearch = 0
    $buildHostedWebSearch = 0
    $buildXSearch = 0
    $proxyAddedWebSearch = $false
    $clientWebSearchAliased = $false
    # The global last_request_meta.json cannot be attributed to a channel when
    # another Grok session is active. Use only channel-scoped log evidence from
    # this probe's time window so concurrent requests cannot create false hits.
    $escapedChannel = [Regex]::Escape($model)
    $channelRequestLines = @($newLogLines | Where-Object {
        $_ -match "UP channel=$escapedChannel\s+(?:backend|incoming)="
    })
    $channelRequestLine = if ($searchProbe) {
        $channelRequestLines | Where-Object {
            $_ -match '\sweb_search=[1-9][0-9]*'
        } | Select-Object -First 1
    } else {
        $channelRequestLines | Select-Object -First 1
    }
    if ($null -ne $channelRequestLine) {
        $line = [string]$channelRequestLine
        $proxyHit = $true
        if ($line -match '\smodel=([^\s]+)') { $requestModel = $Matches[1] }
        if ($line -match '\stools=([0-9]+)') { $toolCount = [int]$Matches[1] }
        if ($line -match '\sweb_search=([0-9]+)') { $webSearch = [int]$Matches[1] }
        if ($line -match '\shosted_web_search=([0-9]+)') { $hostedWebSearch = [int]$Matches[1] }
        if ($line -match '\sfunction_web_search=([0-9]+)') { $functionWebSearch = [int]$Matches[1] }
        if ($line -match '\sx_search=([0-9]+)') { $xSearch = [int]$Matches[1] }
        if ($line -match '\sbuild_hosted_web_search=([0-9]+)') { $buildHostedWebSearch = [int]$Matches[1] }
        if ($line -match '\sbuild_x_search=([0-9]+)') { $buildXSearch = [int]$Matches[1] }
        if ($line -match '\sproxy_added_web_search=(true|false)') { $proxyAddedWebSearch = $Matches[1] -eq "true" }
        if ($line -match '\sclient_web_search_aliased=(true|false)') { $clientWebSearchAliased = $Matches[1] -eq "true" }
    }

    $backendCall = $false
    $backendResult = $false
    $searchSourceURLs = [Collections.Generic.HashSet[string]]::new([StringComparer]::OrdinalIgnoreCase)
    $proxySearchSources = 0
    $localToolCall = $false
    $localWebSearchCall = $false
    $webFetchCall = $false
    $webFetchResult = $false
    $subagentCall = $false
    $subagentResult = $false
    $subagentCompletedResult = $false
    $subagentWaitResult = $false
    $message = ""
    if ($status -eq "ok") {
        if ($searchProbe -or $RequireWebFetch) {
            $textParts = [Collections.Generic.List[string]]::new()
            $fetchCallIds = [Collections.Generic.HashSet[string]]::new()
            $clientSearchCallIds = [Collections.Generic.HashSet[string]]::new()
            $subagentCallIds = [Collections.Generic.HashSet[string]]::new()
            $subagentWaitCallIds = [Collections.Generic.HashSet[string]]::new()
            Get-Content -LiteralPath $stdout | ForEach-Object {
                try {
                    $event = $_ | ConvertFrom-Json -Depth 100
                    if ($null -ne $event.message) {
                        foreach ($block in @($event.message.content)) {
                            if ($block.type -eq "server_tool_use" -and $block.name -eq "web_search") {
                                $backendCall = $true
                            }
                            if ($block.type -eq "web_search_tool_result") {
                                $backendResult = $true
                                foreach ($entry in @($block.content)) {
                                    $url = [string]$entry.url
                                    if ($url -match '^https?://') {
                                        [void]$searchSourceURLs.Add($url)
                                    }
                                }
                            }
                            foreach ($citation in @($block.citations)) {
                                $url = [string]$citation.url
                                if ($url -match '^https?://') {
                                    [void]$searchSourceURLs.Add($url)
                                }
                            }
                            if ($block.type -eq "tool_use") {
                                if ($block.name -match '^(?i:spawn_subagent|agent|task)$') {
                                    $subagentCall = $true
                                    if ($block.id) { [void]$subagentCallIds.Add([string]$block.id) }
                                }
                                if ($block.name -match '^(?i:get_command_or_subagent_output|get_task_output)$' -and $block.id) {
                                    [void]$subagentWaitCallIds.Add([string]$block.id)
                                }
                                $isResponsesBackend = $block.input.backend -eq $true -or
                                    $block.input.variant -eq "WebSearch" -or
                                    $block.name -eq "Web search:"
                                if ($isResponsesBackend) {
                                    $backendCall = $true
                                } else {
                                    $localToolCall = $true
                                    if ($block.name -eq "web_search") {
                                        $localWebSearchCall = $true
                                    }
                                }
                                if ($block.name -eq "web_search" -and $block.id) {
                                    [void]$clientSearchCallIds.Add([string]$block.id)
                                }
                            }
                            if ($block.type -eq "tool_use" -and $block.name -eq "web_fetch") {
                                $webFetchCall = $true
                                if ($block.id) { [void]$fetchCallIds.Add([string]$block.id) }
                            }
                            if ($block.type -eq "tool_result" -and $fetchCallIds.Contains([string]$block.tool_use_id)) {
                                $webFetchResult = $true
                            }
                            if ($block.type -eq "tool_result" -and $clientSearchCallIds.Contains([string]$block.tool_use_id)) {
                                try {
                                    $clientResult = ([string]$block.content) | ConvertFrom-Json -Depth 100
                                    $clientContent = ([string]$clientResult.content).Trim()
                                    if ($clientResult.type -eq "WebSearch" -and $clientContent -and
                                        $clientContent -ne "No search results found.") {
                                        $backendResult = $true
                                        foreach ($match in [Regex]::Matches($clientContent, 'https?://[^\s<>()\[\]{}"'']+')) {
                                            [void]$searchSourceURLs.Add($match.Value.TrimEnd('.', ',', ';', ':', '!', '?'))
                                        }
                                    }
                                } catch {}
                            }
                            if ($block.type -eq "tool_result" -and $subagentCallIds.Contains([string]$block.tool_use_id)) {
                                $subagentResult = $true
                                try {
                                    $spawnResult = ([string]$block.content) | ConvertFrom-Json -Depth 100
                                    if ($spawnResult.type -eq "SubagentCompleted" -and
                                        -not [string]::IsNullOrWhiteSpace([string]$spawnResult.output)) {
                                        $subagentCompletedResult = $true
                                    }
                                } catch {}
                            }
                            if ($block.type -eq "tool_result" -and $subagentWaitCallIds.Contains([string]$block.tool_use_id)) {
                                $subagentWaitResult = $true
                            }
                        }
                    }
                    if ($event.type -eq "result" -and $null -ne $event.result) {
                        $textParts.Add([string]$event.result)
                        if ([int]$event.usage.server_tool_use.web_search_requests -gt 0) {
                            $backendCall = $true
                        }
                    }
                } catch {}
            }
            $message = (($textParts -join "") -replace '\s+', ' ').Trim()

            if ($searchProbe) {
                $evidenceCall = [bool]($newLogLines | Where-Object {
                    $_ -match "UP channel=$escapedChannel search evidence declared=true .*calls=[1-9][0-9]* completed=[1-9][0-9]*"
                })
                $evidenceResult = [bool]($newLogLines | Where-Object {
                    $_ -match "UP channel=$escapedChannel search evidence declared=true .*sources=[1-9][0-9]*" -or
                    $_ -match "UP channel=$escapedChannel search evidence declared=true .*annotations=[1-9][0-9]*"
                })
                foreach ($line in @($newLogLines | Where-Object {
                    $_ -match "UP channel=$escapedChannel search evidence declared=true"
                })) {
                    $sources = if ($line -match '\ssources=([0-9]+)') { [int]$Matches[1] } else { 0 }
                    $annotations = if ($line -match '\sannotations=([0-9]+)') { [int]$Matches[1] } else { 0 }
                    $proxySearchSources = [Math]::Max($proxySearchSources, [Math]::Max($sources, $annotations))
                }
                $backendCall = $backendCall -or $evidenceCall
                $backendResult = $backendResult -or $evidenceResult
                $searchSourceCount = [Math]::Max($searchSourceURLs.Count, $proxySearchSources)

                $answerMatches = $message -match '(?i)grok-build' -and
                    $message -match '(?i)(?<![0-9a-f])[0-9a-f]{7}(?![0-9a-f])'
                if ($RequireSubagentSearch) {
                    $parentDidNotStealSearch = $channelRequestLines.Count -gt 0 -and
                        [string]$channelRequestLines[0] -match 'client_web_search_prepared=false'
                    # Grok Build intentionally excludes project-layer
                    # [subagents.models] from trust-independent model
                    # resolution. Cross-model probes must therefore configure
                    # the real GROK_HOME first; this parameter only verifies
                    # which child channel was actually observed.
                    $expectedChildModel = if ($ExpectedSubagentModel) { $ExpectedSubagentModel } else { $model }
                    $escapedChildModel = [Regex]::Escape($expectedChildModel)
                    $childRequestLines = @($newLogLines | Where-Object {
                        $_ -match "UP channel=$escapedChildModel\s+(?:backend|incoming)="
                    })
                    if ($expectedChildModel -eq $model) {
                        $childRequestLines = @($childRequestLines | Select-Object -Skip 1)
                    }
                    $childClientSearch = [bool](@($childRequestLines | Where-Object {
                        $_ -match 'client_web_search_prepared=true'
                    }).Count)
                    $childHostedSearch = [bool]($newLogLines | Where-Object {
                        $_ -match "UP channel=$escapedChildModel search evidence declared=true .*calls=[1-9][0-9]* completed=[1-9][0-9]*"
                    })
                    $subagentCompletionObserved = $subagentWaitResult -or $subagentCompletedResult
                    if ($childRequestLines.Count -eq 0 -or -not $subagentCall -or -not $subagentResult -or
                        -not $subagentCompletionObserved -or -not $parentDidNotStealSearch -or -not $backendResult -or
                        -not $answerMatches -or (-not $childClientSearch -and -not $childHostedSearch)) {
                        $status = "failed"
                        $message = "parent did not delegate verifiable web_search to expected child model $expectedChildModel"
                    }
                } else {
                    $isGrokRoute = $model -match '(?i)(^|/)grok(?:$|[-_])' -or
                        $requestModel -match '(?i)(^|/)grok(?:$|[-_])'
                    $searchDialectMatches = if ($isGrokRoute) { $xSearch -eq 1 } else { $xSearch -eq 0 }
                    $wireIsHosted = $proxyHit -and $hostedWebSearch -eq 1 -and
                        $functionWebSearch -eq 0 -and $searchDialectMatches -and
                        (($buildHostedWebSearch + $buildXSearch) -ge 1 -or $proxyAddedWebSearch)
                    $wireIsClient = $proxyHit -and $hostedWebSearch -eq 0 -and
                        $functionWebSearch -eq 1 -and $xSearch -eq 0 -and -not $proxyAddedWebSearch -and
                        $clientWebSearchAliased
                    $executionMatches = if ($wireIsHosted) {
                        $backendCall -and $backendResult -and $searchSourceCount -gt 0 -and -not $localWebSearchCall
                    } else {
                        ($backendCall -or $localToolCall) -and $backendResult -and $searchSourceCount -gt 0
                    }
                    if ((-not $wireIsHosted -and -not $wireIsClient) -or -not $executionMatches -or
                        -not $answerMatches) {
                        $status = "failed"
                        $message = "web_search did not use a valid hosted or client-search path with source evidence"
                    }
                }
            } else {
                $proxyFetchCall = [bool]($newLogLines | Where-Object {
                    $_ -match "UP channel=$escapedChannel search evidence .*web_fetch_calls=[1-9][0-9]*"
                })
                $webFetchCall = $webFetchCall -or $proxyFetchCall
                $answerMatches = $message -match '(?i)example domain'
                if (-not $webFetchCall -or -not $webFetchResult -or -not $answerMatches) {
                    $status = "failed"
                    $message = "Build-local web_fetch did not complete with a tool result"
                }
            }
        } else {
            try {
                $result = Get-Content -Raw -LiteralPath $stdout | ConvertFrom-Json -Depth 100
                $message = ([string]$result.text -replace '\s+', ' ').Trim()
                if ($Prompt -eq "Reply with exactly: OK. Do not call tools." -and $message -notmatch '^(?i:OK)[.!]?$') {
                    $status = "failed"
                    $message = "unexpected reply to exact-OK probe: $message"
                }
            } catch {
                $message = "invalid JSON output"
                $status = "failed"
            }
        }
    } elseif ($status -eq "timeout") {
        $message = "timeout after $TimeoutSeconds seconds"
    } else {
        $lastError = Get-Content -LiteralPath $stderr -Tail 1 -ErrorAction SilentlyContinue
        if (-not $lastError) {
            $lastError = Get-Content -LiteralPath $stdout -Tail 1 -ErrorAction SilentlyContinue
        }
        $message = if ($null -eq $lastError) { "no error output" } else { ([string]$lastError).Trim() }
    }
    if ($message.Length -gt 100) {
        $message = $message.Substring(0, 100)
    }

    [pscustomobject]@{
        Channel = $model
        Status = $status
        Exit = $exitCode
        ProxyHit = $proxyHit
        RequestModel = $requestModel
        Tools = $toolCount
        WebSearch = $webSearch
        HostedWebSearch = $hostedWebSearch
        FunctionWebSearch = $functionWebSearch
        XSearch = $xSearch
        BuildHostedWebSearch = $buildHostedWebSearch
        BuildXSearch = $buildXSearch
        ProxyAddedWebSearch = $proxyAddedWebSearch
        ClientWebSearchAliased = $clientWebSearchAliased
        BackendCall = $backendCall
        BackendResult = $backendResult
        SearchSources = [Math]::Max($searchSourceURLs.Count, $proxySearchSources)
        LocalToolCall = $localToolCall
        WebFetchCall = $webFetchCall
        WebFetchResult = $webFetchResult
        SubagentCall = $subagentCall
        SubagentResult = $subagentResult
        Reply = $message
    }
}

$results | Format-Table -AutoSize
if ($results.Status -contains "failed" -or $results.Status -contains "timeout") {
    exit 1
}
exit 0
