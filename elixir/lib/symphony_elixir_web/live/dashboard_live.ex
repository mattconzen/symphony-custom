defmodule SymphonyElixirWeb.DashboardLive do
  @moduledoc """
  Live observability dashboard for Symphony.
  """

  use Phoenix.LiveView, layout: {SymphonyElixirWeb.Layouts, :app}

  alias SymphonyElixir.WorkItems
  alias SymphonyElixirWeb.{Endpoint, ObservabilityPubSub, Presenter}
  @runtime_tick_ms 1_000

  @kanban_columns [
    {"Backlog", "Task exists, but needs an OpenSpec spec."},
    {"Ready", "Spec defined; ready for an agent to pick up."},
    {"In Progress", "Agents are working on the task."},
    {"In Review", "Agents finished; PR awaiting review."},
    {"Done", "PR merged."}
  ]

  @impl true
  def mount(_params, _session, socket) do
    socket =
      socket
      |> assign(:payload, load_payload())
      |> assign(:now, DateTime.utc_now())
      |> assign(:connected, connected?(socket))
      |> assign(:work_items, WorkItems.list())
      |> assign(:active_modal, nil)
      |> assign(:work_item_form_error, nil)
      |> assign(:editing_item_id, nil)
      |> assign(:spec_draft, "")
      |> assign(:spec_modal_mode, :view)

    if connected?(socket) do
      :ok = ObservabilityPubSub.subscribe()
      schedule_runtime_tick()
    end

    {:ok, socket}
  end

  @impl true
  def handle_event("open_new_work_item", _params, socket) do
    {:noreply,
     socket
     |> assign(:active_modal, :new_work_item)
     |> assign(:work_item_form_error, nil)}
  end

  def handle_event("close_modal", _params, socket) do
    {:noreply, close_modal(socket)}
  end

  def handle_event("create_work_item", params, socket) do
    title = params |> Map.get("title", "") |> to_string() |> String.trim()
    body = params |> Map.get("body", "") |> to_string()

    case WorkItems.create(%{"title" => title, "body" => body}) do
      {:ok, _item} ->
        {:noreply,
         socket
         |> assign(:work_items, WorkItems.list())
         |> close_modal()}

      {:error, :title_required} ->
        {:noreply, assign(socket, :work_item_form_error, "Title is required")}

      {:error, reason} ->
        {:noreply, assign(socket, :work_item_form_error, "Could not create work item: #{inspect(reason)}")}
    end
  end

  def handle_event("view_spec", %{"id" => id}, socket) do
    case find_work_item(socket, id) do
      nil ->
        {:noreply, socket}

      item ->
        {:noreply,
         socket
         |> assign(:active_modal, :spec)
         |> assign(:editing_item_id, item.id)
         |> assign(:spec_draft, item.spec || "")
         |> assign(:spec_modal_mode, :view)}
    end
  end

  def handle_event("edit_spec", %{"id" => id}, socket) do
    case find_work_item(socket, id) do
      nil ->
        {:noreply, socket}

      item ->
        {:noreply,
         socket
         |> assign(:active_modal, :spec)
         |> assign(:editing_item_id, item.id)
         |> assign(:spec_draft, item.spec || "")
         |> assign(:spec_modal_mode, :edit)}
    end
  end

  def handle_event("save_spec", params, socket) do
    id = socket.assigns.editing_item_id
    spec = params |> Map.get("spec", "") |> to_string()

    case id && WorkItems.update_spec(id, spec) do
      {:ok, _item} ->
        {:noreply,
         socket
         |> assign(:work_items, WorkItems.list())
         |> close_modal()}

      _ ->
        {:noreply, close_modal(socket)}
    end
  end

  def handle_event("advance_state", %{"id" => id, "to" => to_state}, socket) do
    case WorkItems.set_state(id, to_state) do
      {:ok, _item} ->
        {:noreply, assign(socket, :work_items, WorkItems.list())}

      _ ->
        {:noreply, socket}
    end
  end

  @impl true
  def handle_info(:runtime_tick, socket) do
    schedule_runtime_tick()
    {:noreply, assign(socket, :now, DateTime.utc_now())}
  end

  @impl true
  def handle_info(:observability_updated, socket) do
    {:noreply,
     socket
     |> assign(:payload, load_payload())
     |> assign(:work_items, WorkItems.list())
     |> assign(:now, DateTime.utc_now())}
  end

  @impl true
  def render(assigns) do
    ~H"""
    <section class="dashboard-shell">
      <header class="hero-card">
        <div class="hero-grid">
          <div>
            <p class="eyebrow">
              Symphony Observability
            </p>
            <h1 class="hero-title">
              Operations Dashboard
            </h1>
            <p class="hero-copy">
              Current state, retry pressure, token usage, and orchestration health for the active Symphony runtime.
            </p>
          </div>

          <div class="status-stack">
            <%= if @connected do %>
              <span class="status-badge status-badge-live">
                <span class="status-badge-dot"></span>
                Live
              </span>
            <% else %>
              <span class="status-badge status-badge-offline">
                <span class="status-badge-dot"></span>
                Offline
              </span>
            <% end %>
          </div>
        </div>
      </header>

      <section class="section-card kanban-section">
        <div class="section-header">
          <div>
            <h2 class="section-title">Work items</h2>
            <p class="section-copy">Kanban board for tasks tracked by this Symphony runtime.</p>
          </div>

          <button
            type="button"
            class="primary-button"
            phx-click="open_new_work_item"
          >
            New Work Item
          </button>
        </div>

        <div class="kanban-board">
          <div :for={{column_state, column_copy} <- kanban_columns()} class="kanban-column">
            <header class="kanban-column-header">
              <h3 class="kanban-column-title"><%= column_state %></h3>
              <span class="kanban-column-count numeric"><%= length(work_items_for_column(@work_items, column_state)) %></span>
            </header>
            <p class="kanban-column-copy"><%= column_copy %></p>

            <ul class="kanban-card-list">
              <li :for={item <- work_items_for_column(@work_items, column_state)} class="kanban-card">
                <p class="kanban-card-title"><%= item.title %></p>
                <%= if item.body && item.body != "" do %>
                  <p class="kanban-card-body"><%= summarize(item.body) %></p>
                <% end %>
                <%= if item.pr_url do %>
                  <p class="kanban-card-meta">
                    <a class="issue-link" href={item.pr_url} target="_blank" rel="noopener">PR <%= if item.pr_merged, do: "(merged)", else: "(open)" %></a>
                  </p>
                <% end %>

                <div class="kanban-card-actions">
                  <%= if item.spec do %>
                    <button type="button" class="subtle-button" phx-click="view_spec" phx-value-id={item.id}>View OpenSpec</button>
                  <% end %>
                  <button type="button" class="subtle-button" phx-click="edit_spec" phx-value-id={item.id}>
                    <%= if item.spec, do: "Edit OpenSpec", else: "Add OpenSpec" %>
                  </button>
                  <%= for next_state <- next_states(item.state) do %>
                    <button
                      type="button"
                      class="subtle-button"
                      phx-click="advance_state"
                      phx-value-id={item.id}
                      phx-value-to={next_state}
                    >&rarr; <%= next_state %></button>
                  <% end %>
                </div>
              </li>
            </ul>

            <%= if work_items_for_column(@work_items, column_state) == [] do %>
              <p class="empty-state">No items.</p>
            <% end %>
          </div>
        </div>
      </section>

      <%= if @active_modal == :new_work_item do %>
        <div class="modal-overlay" phx-click="close_modal">
          <div class="modal-card" onclick="event.stopPropagation();">
            <header class="modal-header">
              <h2 class="modal-title">New Work Item</h2>
              <button type="button" class="subtle-button" phx-click="close_modal">Close</button>
            </header>

            <form phx-submit="create_work_item" class="modal-form">
              <label class="modal-label" for="new-work-item-title">Title</label>
              <input id="new-work-item-title" name="title" type="text" class="modal-input" autofocus />

              <label class="modal-label" for="new-work-item-body">Markdown task</label>
              <textarea id="new-work-item-body" name="body" class="modal-textarea" rows="12" placeholder="# Describe the task in Markdown..."></textarea>

              <%= if @work_item_form_error do %>
                <p class="modal-error"><%= @work_item_form_error %></p>
              <% end %>

              <div class="modal-actions">
                <button type="button" class="subtle-button" phx-click="close_modal">Cancel</button>
                <button type="submit" class="primary-button">Create</button>
              </div>
            </form>
          </div>
        </div>
      <% end %>

      <%= if @active_modal == :spec do %>
        <div class="modal-overlay" phx-click="close_modal">
          <div class="modal-card" onclick="event.stopPropagation();">
            <header class="modal-header">
              <h2 class="modal-title">
                <%= if @spec_modal_mode == :edit, do: "Edit OpenSpec", else: "View OpenSpec" %>
              </h2>
              <button type="button" class="subtle-button" phx-click="close_modal">Close</button>
            </header>

            <%= if @spec_modal_mode == :view do %>
              <pre class="code-panel"><%= if @spec_draft == "", do: "No spec defined.", else: @spec_draft %></pre>
              <div class="modal-actions">
                <button
                  type="button"
                  class="primary-button"
                  phx-click="edit_spec"
                  phx-value-id={@editing_item_id}
                >Edit</button>
                <button type="button" class="subtle-button" phx-click="close_modal">Close</button>
              </div>
            <% else %>
              <form phx-submit="save_spec" class="modal-form">
                <label class="modal-label" for="spec-draft">OpenSpec (Markdown)</label>
                <textarea id="spec-draft" name="spec" class="modal-textarea" rows="16"><%= @spec_draft %></textarea>

                <div class="modal-actions">
                  <button type="button" class="subtle-button" phx-click="close_modal">Cancel</button>
                  <button type="submit" class="primary-button">Save</button>
                </div>
              </form>
            <% end %>
          </div>
        </div>
      <% end %>

      <%= if @payload[:error] do %>
        <section class="error-card">
          <h2 class="error-title">
            Snapshot unavailable
          </h2>
          <p class="error-copy">
            <strong><%= @payload.error.code %>:</strong> <%= @payload.error.message %>
          </p>
        </section>
      <% else %>
        <section class="metric-grid">
          <article class="metric-card">
            <p class="metric-label">Running</p>
            <p class="metric-value numeric"><%= @payload.counts.running %></p>
            <p class="metric-detail">Active issue sessions in the current runtime.</p>
          </article>

          <article class="metric-card">
            <p class="metric-label">Retrying</p>
            <p class="metric-value numeric"><%= @payload.counts.retrying %></p>
            <p class="metric-detail">Issues waiting for the next retry window.</p>
          </article>

          <article class="metric-card">
            <p class="metric-label">Total tokens</p>
            <p class="metric-value numeric"><%= format_int(@payload.codex_totals.total_tokens) %></p>
            <p class="metric-detail numeric">
              In <%= format_int(@payload.codex_totals.input_tokens) %> / Out <%= format_int(@payload.codex_totals.output_tokens) %>
            </p>
          </article>

          <article class="metric-card">
            <p class="metric-label">Runtime</p>
            <p class="metric-value numeric"><%= format_runtime_seconds(total_runtime_seconds(@payload, @now)) %></p>
            <p class="metric-detail">Total agent runtime across completed and active sessions.</p>
          </article>
        </section>

        <section class="section-card">
          <div class="section-header">
            <div>
              <h2 class="section-title">Rate limits</h2>
              <p class="section-copy">Latest upstream rate-limit snapshot, when available.</p>
            </div>
          </div>

          <pre class="code-panel"><%= pretty_value(@payload.rate_limits) %></pre>
        </section>

        <section class="section-card">
          <div class="section-header">
            <div>
              <h2 class="section-title">Running sessions</h2>
              <p class="section-copy">Active issues, last known agent activity, and token usage.</p>
            </div>
          </div>

          <%= if @payload.running == [] do %>
            <p class="empty-state">No active sessions.</p>
          <% else %>
            <div class="table-wrap">
              <table class="data-table data-table-running">
                <colgroup>
                  <col style="width: 12rem;" />
                  <col style="width: 8rem;" />
                  <col style="width: 7.5rem;" />
                  <col style="width: 8.5rem;" />
                  <col />
                  <col style="width: 10rem;" />
                </colgroup>
                <thead>
                  <tr>
                    <th>Issue</th>
                    <th>State</th>
                    <th>Session</th>
                    <th>Runtime / turns</th>
                    <th>Agent update</th>
                    <th>Tokens</th>
                  </tr>
                </thead>
                <tbody>
                  <tr :for={entry <- @payload.running}>
                    <td>
                      <div class="issue-stack">
                        <span class="issue-id"><%= entry.issue_identifier %></span>
                        <a class="issue-link" href={"/api/v1/#{entry.issue_identifier}"}>JSON details</a>
                      </div>
                    </td>
                    <td>
                      <span class={state_badge_class(entry.state)}>
                        <%= entry.state %>
                      </span>
                    </td>
                    <td>
                      <div class="session-stack">
                        <%= if entry.session_id do %>
                          <button
                            type="button"
                            class="subtle-button"
                            data-label="Copy ID"
                            data-copy={entry.session_id}
                            onclick="navigator.clipboard.writeText(this.dataset.copy); this.textContent = 'Copied'; clearTimeout(this._copyTimer); this._copyTimer = setTimeout(() => { this.textContent = this.dataset.label }, 1200);"
                          >
                            Copy ID
                          </button>
                        <% else %>
                          <span class="muted">n/a</span>
                        <% end %>
                      </div>
                    </td>
                    <td class="numeric"><%= format_runtime_and_turns(entry.started_at, entry.turn_count, @now) %></td>
                    <td>
                      <div class="detail-stack">
                        <span
                          class="event-text"
                          title={entry.last_message || to_string(entry.last_event || "n/a")}
                        ><%= entry.last_message || to_string(entry.last_event || "n/a") %></span>
                        <span class="muted event-meta">
                          <%= entry.last_event || "n/a" %>
                          <%= if entry.last_event_at do %>
                            · <span class="mono numeric"><%= entry.last_event_at %></span>
                          <% end %>
                        </span>
                      </div>
                    </td>
                    <td>
                      <div class="token-stack numeric">
                        <span>Total: <%= format_int(entry.tokens.total_tokens) %></span>
                        <span class="muted">In <%= format_int(entry.tokens.input_tokens) %> / Out <%= format_int(entry.tokens.output_tokens) %></span>
                      </div>
                    </td>
                  </tr>
                </tbody>
              </table>
            </div>
          <% end %>
        </section>

        <section class="section-card">
          <div class="section-header">
            <div>
              <h2 class="section-title">Retry queue</h2>
              <p class="section-copy">Issues waiting for the next retry window.</p>
            </div>
          </div>

          <%= if @payload.retrying == [] do %>
            <p class="empty-state">No issues are currently backing off.</p>
          <% else %>
            <div class="table-wrap">
              <table class="data-table" style="min-width: 680px;">
                <thead>
                  <tr>
                    <th>Issue</th>
                    <th>Attempt</th>
                    <th>Due at</th>
                    <th>Error</th>
                  </tr>
                </thead>
                <tbody>
                  <tr :for={entry <- @payload.retrying}>
                    <td>
                      <div class="issue-stack">
                        <span class="issue-id"><%= entry.issue_identifier %></span>
                        <a class="issue-link" href={"/api/v1/#{entry.issue_identifier}"}>JSON details</a>
                      </div>
                    </td>
                    <td><%= entry.attempt %></td>
                    <td class="mono"><%= entry.due_at || "n/a" %></td>
                    <td><%= entry.error || "n/a" %></td>
                  </tr>
                </tbody>
              </table>
            </div>
          <% end %>
        </section>
      <% end %>
    </section>
    """
  end

  defp load_payload do
    Presenter.state_payload(orchestrator(), snapshot_timeout_ms())
  end

  defp close_modal(socket) do
    socket
    |> assign(:active_modal, nil)
    |> assign(:work_item_form_error, nil)
    |> assign(:editing_item_id, nil)
    |> assign(:spec_draft, "")
    |> assign(:spec_modal_mode, :view)
  end

  defp find_work_item(socket, id) do
    socket.assigns
    |> Map.get(:work_items, [])
    |> Enum.find(fn item -> item.id == id end)
  end

  defp kanban_columns, do: @kanban_columns

  defp work_items_for_column(work_items, column_state) when is_list(work_items) do
    Enum.filter(work_items, fn item -> item.state == column_state end)
  end

  defp next_states(current_state) do
    states = WorkItems.states()

    case Enum.find_index(states, &(&1 == current_state)) do
      nil -> []
      idx when idx + 1 < length(states) -> [Enum.at(states, idx + 1)]
      _ -> []
    end
  end

  defp summarize(text) when is_binary(text) do
    trimmed = String.trim(text)

    if String.length(trimmed) > 160 do
      String.slice(trimmed, 0, 160) <> "…"
    else
      trimmed
    end
  end

  defp summarize(_text), do: ""

  defp orchestrator do
    Endpoint.config(:orchestrator) || SymphonyElixir.Orchestrator
  end

  defp snapshot_timeout_ms do
    Endpoint.config(:snapshot_timeout_ms) || 15_000
  end

  defp completed_runtime_seconds(payload) do
    payload.codex_totals.seconds_running || 0
  end

  defp total_runtime_seconds(payload, now) do
    completed_runtime_seconds(payload) +
      Enum.reduce(payload.running, 0, fn entry, total ->
        total + runtime_seconds_from_started_at(entry.started_at, now)
      end)
  end

  defp format_runtime_and_turns(started_at, turn_count, now) when is_integer(turn_count) and turn_count > 0 do
    "#{format_runtime_seconds(runtime_seconds_from_started_at(started_at, now))} / #{turn_count}"
  end

  defp format_runtime_and_turns(started_at, _turn_count, now),
    do: format_runtime_seconds(runtime_seconds_from_started_at(started_at, now))

  defp format_runtime_seconds(seconds) when is_number(seconds) do
    whole_seconds = max(trunc(seconds), 0)
    mins = div(whole_seconds, 60)
    secs = rem(whole_seconds, 60)
    "#{mins}m #{secs}s"
  end

  defp runtime_seconds_from_started_at(%DateTime{} = started_at, %DateTime{} = now) do
    DateTime.diff(now, started_at, :second)
  end

  defp runtime_seconds_from_started_at(started_at, %DateTime{} = now) when is_binary(started_at) do
    case DateTime.from_iso8601(started_at) do
      {:ok, parsed, _offset} -> runtime_seconds_from_started_at(parsed, now)
      _ -> 0
    end
  end

  defp runtime_seconds_from_started_at(_started_at, _now), do: 0

  defp format_int(value) when is_integer(value) do
    value
    |> Integer.to_string()
    |> String.reverse()
    |> String.replace(~r/.{3}(?=.)/, "\\0,")
    |> String.reverse()
  end

  defp format_int(_value), do: "n/a"

  defp state_badge_class(state) do
    base = "state-badge"
    normalized = state |> to_string() |> String.downcase()

    cond do
      String.contains?(normalized, ["progress", "running", "active"]) -> "#{base} state-badge-active"
      String.contains?(normalized, ["blocked", "error", "failed"]) -> "#{base} state-badge-danger"
      String.contains?(normalized, ["todo", "queued", "pending", "retry"]) -> "#{base} state-badge-warning"
      true -> base
    end
  end

  defp schedule_runtime_tick do
    Process.send_after(self(), :runtime_tick, @runtime_tick_ms)
  end

  defp pretty_value(nil), do: "n/a"
  defp pretty_value(value), do: inspect(value, pretty: true, limit: :infinity)
end
