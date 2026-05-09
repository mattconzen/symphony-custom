defmodule SymphonyElixir.WorkItems do
  @moduledoc """
  In-memory store of dashboard-created work items.

  Each work item is a Markdown task with optional OpenSpec spec, a Kanban
  state, and optional pull-request metadata. The dashboard reads from and
  writes to this store via the LiveView. Updates are broadcast on the
  observability PubSub topic so the dashboard re-renders.
  """

  use GenServer

  alias SymphonyElixirWeb.ObservabilityPubSub

  @backlog "Backlog"
  @ready "Ready"
  @in_progress "In Progress"
  @in_review "In Review"
  @done "Done"

  @states [@backlog, @ready, @in_progress, @in_review, @done]

  @type id :: String.t()
  @type t :: %{
          id: id(),
          title: String.t(),
          body: String.t(),
          spec: String.t() | nil,
          state: String.t(),
          pr_url: String.t() | nil,
          pr_merged: boolean(),
          created_at: DateTime.t()
        }

  @spec start_link(keyword()) :: GenServer.on_start()
  def start_link(opts \\ []) do
    name = Keyword.get(opts, :name, __MODULE__)
    GenServer.start_link(__MODULE__, opts, name: name)
  end

  @spec states() :: [String.t()]
  def states, do: @states

  @spec backlog_state() :: String.t()
  def backlog_state, do: @backlog

  @spec ready_state() :: String.t()
  def ready_state, do: @ready

  @spec in_progress_state() :: String.t()
  def in_progress_state, do: @in_progress

  @spec in_review_state() :: String.t()
  def in_review_state, do: @in_review

  @spec done_state() :: String.t()
  def done_state, do: @done

  @spec list() :: [t()]
  def list, do: list(__MODULE__)

  @spec list(GenServer.server()) :: [t()]
  def list(server) do
    case GenServer.whereis(server) do
      pid when is_pid(pid) -> GenServer.call(server, :list)
      _ -> []
    end
  end

  @spec create(map()) :: {:ok, t()} | {:error, atom()}
  def create(params) when is_map(params), do: create(__MODULE__, params)

  @spec create(GenServer.server(), map()) :: {:ok, t()} | {:error, atom()}
  def create(server, params) when is_map(params) do
    GenServer.call(server, {:create, params})
  end

  @spec update_spec(id(), String.t() | nil) :: {:ok, t()} | {:error, atom()}
  def update_spec(id, spec) when is_binary(id), do: update_spec(__MODULE__, id, spec)

  @spec update_spec(GenServer.server(), id(), String.t() | nil) ::
          {:ok, t()} | {:error, atom()}
  def update_spec(server, id, spec) when is_binary(id) do
    GenServer.call(server, {:update_spec, id, spec})
  end

  @spec set_state(id(), String.t()) :: {:ok, t()} | {:error, atom()}
  def set_state(id, state) when is_binary(id) and is_binary(state),
    do: set_state(__MODULE__, id, state)

  @spec set_state(GenServer.server(), id(), String.t()) ::
          {:ok, t()} | {:error, atom()}
  def set_state(server, id, state) when is_binary(id) and is_binary(state) do
    GenServer.call(server, {:set_state, id, state})
  end

  @impl true
  def init(_opts) do
    {:ok, %{items: []}}
  end

  @impl true
  def handle_call(:list, _from, state) do
    {:reply, state.items, state}
  end

  def handle_call({:create, params}, _from, state) do
    title =
      params
      |> Map.get("title", Map.get(params, :title, ""))
      |> to_string()
      |> String.trim()

    body =
      params
      |> Map.get("body", Map.get(params, :body, ""))
      |> to_string()

    if title == "" do
      {:reply, {:error, :title_required}, state}
    else
      item = %{
        id: random_id(),
        title: title,
        body: body,
        spec: nil,
        state: @backlog,
        pr_url: nil,
        pr_merged: false,
        created_at: DateTime.utc_now()
      }

      new_state = %{state | items: state.items ++ [item]}
      ObservabilityPubSub.broadcast_update()
      {:reply, {:ok, item}, new_state}
    end
  end

  def handle_call({:update_spec, id, spec}, _from, state) do
    case find_index(state.items, id) do
      nil ->
        {:reply, {:error, :not_found}, state}

      idx ->
        existing = Enum.at(state.items, idx)
        normalized = normalize_spec(spec)

        next_state =
          cond do
            is_nil(normalized) and existing.state == @ready -> @backlog
            not is_nil(normalized) and existing.state == @backlog -> @ready
            true -> existing.state
          end

        updated = %{existing | spec: normalized, state: next_state}
        new_items = List.replace_at(state.items, idx, updated)
        ObservabilityPubSub.broadcast_update()
        {:reply, {:ok, updated}, %{state | items: new_items}}
    end
  end

  def handle_call({:set_state, id, new_state_value}, _from, state) do
    cond do
      new_state_value not in @states ->
        {:reply, {:error, :invalid_state}, state}

      true ->
        case find_index(state.items, id) do
          nil ->
            {:reply, {:error, :not_found}, state}

          idx ->
            existing = Enum.at(state.items, idx)
            updated = %{existing | state: new_state_value}
            new_items = List.replace_at(state.items, idx, updated)
            ObservabilityPubSub.broadcast_update()
            {:reply, {:ok, updated}, %{state | items: new_items}}
        end
    end
  end

  defp find_index(items, id) do
    Enum.find_index(items, fn item -> item.id == id end)
  end

  defp normalize_spec(nil), do: nil

  defp normalize_spec(spec) when is_binary(spec) do
    case String.trim(spec) do
      "" -> nil
      _ -> spec
    end
  end

  defp random_id do
    :crypto.strong_rand_bytes(8) |> Base.url_encode64(padding: false)
  end
end
