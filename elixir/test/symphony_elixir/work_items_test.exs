defmodule SymphonyElixir.WorkItemsTest do
  use ExUnit.Case, async: true

  alias SymphonyElixir.WorkItems

  setup do
    name = String.to_atom("work_items_test_#{System.unique_integer([:positive])}")
    pid = start_supervised!({WorkItems, name: name})
    {:ok, server: name, pid: pid}
  end

  test "list/1 returns empty when no items", %{server: server} do
    assert WorkItems.list(server) == []
  end

  test "create/2 adds an item with default Backlog state", %{server: server} do
    {:ok, item} = WorkItems.create(server, %{"title" => "Pick up groceries", "body" => "milk\neggs"})

    assert item.title == "Pick up groceries"
    assert item.body == "milk\neggs"
    assert item.spec == nil
    assert item.state == WorkItems.backlog_state()
    assert item.pr_url == nil
    refute item.pr_merged
    assert is_binary(item.id)
    assert %DateTime{} = item.created_at

    assert [^item] = WorkItems.list(server)
  end

  test "create/2 rejects empty titles", %{server: server} do
    assert {:error, :title_required} = WorkItems.create(server, %{"title" => ""})
    assert {:error, :title_required} = WorkItems.create(server, %{"title" => "   "})
    assert {:error, :title_required} = WorkItems.create(server, %{})
    assert WorkItems.list(server) == []
  end

  test "update_spec/3 transitions Backlog → Ready when spec is added", %{server: server} do
    {:ok, item} = WorkItems.create(server, %{"title" => "Build it"})
    assert item.state == WorkItems.backlog_state()

    {:ok, updated} = WorkItems.update_spec(server, item.id, "## Spec\nDo the thing.")
    assert updated.spec == "## Spec\nDo the thing."
    assert updated.state == WorkItems.ready_state()
  end

  test "update_spec/3 transitions Ready → Backlog when spec is cleared", %{server: server} do
    {:ok, item} = WorkItems.create(server, %{"title" => "Build it"})
    {:ok, _} = WorkItems.update_spec(server, item.id, "spec body")

    {:ok, cleared} = WorkItems.update_spec(server, item.id, "   ")
    assert cleared.spec == nil
    assert cleared.state == WorkItems.backlog_state()

    {:ok, also_cleared} = WorkItems.update_spec(server, item.id, nil)
    assert also_cleared.spec == nil
    assert also_cleared.state == WorkItems.backlog_state()
  end

  test "update_spec/3 leaves non-default states unchanged", %{server: server} do
    {:ok, item} = WorkItems.create(server, %{"title" => "Build it"})
    {:ok, _} = WorkItems.set_state(server, item.id, WorkItems.in_progress_state())

    {:ok, updated} = WorkItems.update_spec(server, item.id, "spec")
    assert updated.spec == "spec"
    assert updated.state == WorkItems.in_progress_state()
  end

  test "update_spec/3 returns :not_found for unknown ids", %{server: server} do
    assert {:error, :not_found} = WorkItems.update_spec(server, "missing", "x")
  end

  test "set_state/3 validates the target state", %{server: server} do
    {:ok, item} = WorkItems.create(server, %{"title" => "Build it"})

    for valid <- WorkItems.states() do
      assert {:ok, %{state: ^valid}} = WorkItems.set_state(server, item.id, valid)
    end

    assert {:error, :invalid_state} = WorkItems.set_state(server, item.id, "Bogus")
    assert {:error, :not_found} = WorkItems.set_state(server, "missing", WorkItems.done_state())
  end

  test "states/0 enumerates the kanban columns" do
    assert WorkItems.states() == [
             WorkItems.backlog_state(),
             WorkItems.ready_state(),
             WorkItems.in_progress_state(),
             WorkItems.in_review_state(),
             WorkItems.done_state()
           ]
  end

  test "list/0 returns [] when the GenServer is not running" do
    name = String.to_atom("work_items_missing_#{System.unique_integer([:positive])}")
    assert WorkItems.list(name) == []
  end

  test "default-name wrappers exercise the application-supervised store" do
    title = "wrapper-#{System.unique_integer([:positive])}"
    {:ok, item} = WorkItems.create(%{"title" => title, "body" => "hi"})

    assert Enum.any?(WorkItems.list(), &(&1.id == item.id))

    assert {:ok, _} = WorkItems.update_spec(item.id, "spec body")
    assert {:ok, _} = WorkItems.set_state(item.id, WorkItems.in_progress_state())
  end
end
